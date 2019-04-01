package main

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"time"

	"github.com/olivere/elastic"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

const UpdateInterval time.Duration = 30 * time.Second

type nginxRPS struct {
	Timestamp  string `json:"@timestamp"`
	StatusCode int    `json:"response_status"`
	Host       string `json:"host"`
}

type nginxPercentile struct {
	Percentile map[string]float64
	Host       string
}

type nginxBucket struct {
	RequesTime float64 `json:"request_time"`
}

func check(e error) {
	if e != nil {
		panic(e)
	}
}

func main() {
	searchQueries := make(map[string]map[string][]string)

	data, err := ioutil.ReadFile("settings.yaml")
	check(err)
	// fmt.Print(string(data))
	yaml.Unmarshal([]byte(data), &searchQueries)
	// fmt.Println(searchQueries)

	ESHost := "http://10.193.16.76:30111"
	ctx := context.Background()
	log.Info("Connecting to ElasticSearch..")
	var client *elastic.Client
	for {
		esClient, err := elastic.NewClient(elastic.SetURL(ESHost), elastic.SetSniff(false))
		if err != nil {
			log.Errorf("Could not connect to ElasticSearch: %v\n", err)
			time.Sleep(1 * time.Second)
			continue
		}
		client = esClient
		break
	}

	info, _, err := client.Ping(ESHost).Do(ctx)
	if err != nil {
		log.Fatalf("Could not ping ElasticSearch %v", err)
	}
	log.Infof("Connected to ElasticSerach with version %s\n", info.Version.Number)

	StatusCodeCollector := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "responses"}, []string{"host", "code"})
	PercentileCollector := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "Percentile",
		},
		[]string{
			"host",
			"Percentile",
		},
	)
	HistogramCollector := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "histogram",
		Buckets: prometheus.ExponentialBuckets(0.01, 2.6, 10),
	}, []string{"host", "searchstring"})

	r := prometheus.NewRegistry()
	r.MustRegister(StatusCodeCollector)
	r.MustRegister(PercentileCollector)
	r.MustRegister(HistogramCollector)
	handler := promhttp.HandlerFor(r, promhttp.HandlerOpts{})

	ElasticGetRps(ctx, client, StatusCodeCollector)

	for host, _ := range searchQueries["percentiles"] {
		ElasticGetPercentile(ctx, client, host, PercentileCollector)

	}

	for host, _ := range searchQueries["percentiles"] {
		for _, searchString := range searchQueries["percentiles"][host] {
			ElasticGetBucket(ctx, client, host, searchString, HistogramCollector)
		}
	}

	http.Handle("/metrics", handler)
	log.Fatal(http.ListenAndServe(":8092", nil))

}

func ElasticGetRps(ctx context.Context, client *elastic.Client, StatusCodeCollector *prometheus.CounterVec) {

	// UpdateInterval := 10 * time.Second
	ticker := time.NewTicker(UpdateInterval)
	go func() {
		for range ticker.C {

			now := time.Now()
			prev := now.Add(-UpdateInterval)

			query := elastic.NewBoolQuery()
			q1 := elastic.NewRangeQuery("response_status").Gte(100).Lte(600)
			q2 := elastic.NewRangeQuery("timestamp").Gte(prev.Unix()).Lte(now.Unix()).Format("epoch_second")
			query.Must(q2, q1)
			scroll := client.Scroll("*").
				Query(query).
				Size(5000)

			scrollIdx := 0
			for {
				res, err := scroll.Do(ctx)
				if err == io.EOF {
					break
				}
				if err != nil {
					log.Errorf("Error while fetching from ElasticSearch: %v", err)
					break
				}
				scrollIdx++
				// log.Infof("Query Executed, Hits: %d TookInMillis: %d Now: %s Prev: %s", res.TotalHits(), res.TookInMillis, now.Format("2006-01-02 15:04:05"), prev.Format("2006-01-02 15:04:05"))

				var ttyp nginxRPS
				for _, item := range res.Each(reflect.TypeOf(ttyp)) {
					t := item.(nginxRPS)
					trackStatusCodes(t.StatusCode, t.Host, StatusCodeCollector)
				}
			}
		}
	}()
}

func ElasticGetPercentile(ctx context.Context, client *elastic.Client, host string, PercentileCollector *prometheus.GaugeVec) {

	// UpdateInterval := 10 * time.Second
	ticker := time.NewTicker(UpdateInterval)
	go func() {
		for range ticker.C {

			now := time.Now()
			prev := now.Add(-UpdateInterval)
			// fmt.Println(prev)

			query := elastic.NewBoolQuery()
			q1 := elastic.NewTermsQuery("host", host)
			q2 := elastic.NewRangeQuery("timestamp").Gte(prev.Unix()).Lte(now.Unix()).Format("epoch_second")
			q3 := elastic.NewRegexpQuery("request", ".*(jpg|jpeg|gif|png|ico|css|bmp|js|swf|htc|doc|docx|pdf|mp4|rtf|odt|odf|eot|ttf|woff|ipa|svg|zip).*")
			query.Must(q1, q2)
			query.MustNot(q3)
			aggs := elastic.NewPercentilesAggregation().Field("request_time").Percentiles(50, 95, 99)

			res, err := client.Search().
				Index("*"). // search in index "twitter"
				Query(query).
				Aggregation("Percentile", aggs).
				Do(ctx) // execute
			if err != nil {
				// Handle error
				// panic(err)
				log.Println(err)
			}

			// Dump result as json
			// data, _ := json.Marshal(res)
			// log.Infoln(string(data))

			percentilesAggRes, found := res.Aggregations.Percentiles("Percentile")
			if !found {
				log.Printf("expected %v; got: %v", true, found)
			}
			if percentilesAggRes == nil {
				log.Fatalf("expected != nil; got: nil")
			}
			if len(percentilesAggRes.Values) == 0 {
				log.Printf("expected at least %d value; got: %d\nValues are: %#v", 1, len(percentilesAggRes.Values), percentilesAggRes.Values)
			}
			// log.Println(percentilesAggRes.Values["99.0"])
			var percentilesList nginxPercentile
			percentilesList.Percentile = percentilesAggRes.Values
			percentilesList.Host = host

			// fmt.Println(p)
			for p, _ := range percentilesList.Percentile {
				trackPercentiles(string(p), percentilesList.Percentile[p], percentilesList.Host, PercentileCollector)
			}
		}

	}()
}

func ElasticGetBucket(ctx context.Context, client *elastic.Client, host string, searchString string, HistogramCollector *prometheus.HistogramVec) {

	ticker := time.NewTicker(UpdateInterval)
	go func() {
		for range ticker.C {

			now := time.Now()
			prev := now.Add(-UpdateInterval)

			query := elastic.NewBoolQuery()
			q1 := elastic.NewTermsQuery("host", host)
			q2 := elastic.NewRangeQuery("timestamp").Gte(prev.Unix()).Lte(now.Unix()).Format("epoch_second")
			q3 := elastic.NewRegexpQuery("request", fmt.Sprintf(`%s`, searchString))
			query.Must(q1, q2, q3)
			scroll := client.Scroll("*").
				Query(query).
				Size(5000)

			scrollIdx := 0
			for {
				res, err := scroll.Do(ctx)
				if err == io.EOF {
					break
				}
				if err != nil {
					log.Errorf("Error while fetching from ElasticSearch: %v", err)
					break
				}
				scrollIdx++
				// log.Infof("Query Executed, Hits: %d TookInMillis: %d Now: %s Prev: %s", res.TotalHits(), res.TookInMillis, now.Format("2006-01-02 15:04:05"), prev.Format("2006-01-02 15:04:05"))

				var ttyp nginxBucket
				for _, item := range res.Each(reflect.TypeOf(ttyp)) {
					t := item.(nginxBucket)
					// log.Println(t)
					trackHistogram(t.RequesTime, host, searchString, HistogramCollector)
				}
			}
		}
	}()
}

func trackPercentiles(Percentile string, pValue float64, Host string, PercentileCollector *prometheus.GaugeVec) {
	PercentileCollector.WithLabelValues(Host, Percentile).Set(pValue)

}

func trackStatusCodes(StatusCode int, Host string, StatusCodeCollector *prometheus.CounterVec) {
	StatusCodeCollector.WithLabelValues(Host, strconv.Itoa(StatusCode)).Inc()
	return
}

func trackHistogram(RequesTime float64, Host string, searchstring string, HistogramCollector *prometheus.HistogramVec) {
	r, _ := regexp.Compile(`\w+`)
	HistogramCollector.WithLabelValues(Host, r.FindString(searchstring)).Observe(RequesTime)

}
