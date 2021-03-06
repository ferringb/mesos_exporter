package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/dgrijalva/jwt-go"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
)

type (
	resources struct {
		CPUs  float64 `json:"cpus"`
		Disk  float64 `json:"disk"`
		Mem   float64 `json:"mem"`
		Ports ranges  `json:"ports"`
	}

	task struct {
		Name        string    `json:"name"`
		ID          string    `json:"id"`
		ExecutorID  string    `json:"executor_id"`
		FrameworkID string    `json:"framework_id"`
		Role        string    `json:"role"`
		SlaveID     string    `json:"slave_id"`
		State       string    `json:"state"`
		Labels      []label   `json:"labels"`
		Resources   resources `json:"resources"`
		Statuses    []status  `json:"statuses"`
	}

	label struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}

	status struct {
		State     string  `json:"state"`
		Timestamp float64 `json:"timestamp"`
	}

	tokenResponse struct {
		Token string `json:"token"`
	}

	tokenRequest struct {
		UID   string `json:"uid"`
		Token string `json:"token"`
	}

	mesosSecret struct {
		LoginEndpoint string `json:"login_endpoint"`
		PrivateKey    string `json:"private_key"`
		Scheme        string `json:"scheme"`
		UID           string `json:"uid"`
	}
)

type groupedCollector struct {
	Collectors []prometheus.Collector
}

func newGroupedCollector(collectors ...prometheus.Collector) prometheus.Collector {
	return &groupedCollector{Collectors: collectors}
}

func (c *groupedCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, collector := range c.Collectors {
		collector.Describe(ch)
	}
}

func (c *groupedCollector) Collect(ch chan<- prometheus.Metric) {
	for _, collector := range c.Collectors {
		collector.Collect(ch)
	}
}

func newStandardCollector(httpClient *httpClient, metrics map[prometheus.Collector]metricsCollectorFunctor) prometheus.Collector {
	return newGroupedCollector(
		newMetricCollector(httpClient, metrics),
		newVersionCollector(httpClient),
	)
}

type metricMap map[string]float64

type metricsCollectorFunctor func(metricMap, prometheus.Collector) error

const LogErrNotFoundInMap = "Couldn't find key in map"

type settableCounterVec struct {
	desc   *prometheus.Desc
	values []prometheus.Metric
}

func (c *settableCounterVec) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

func (c *settableCounterVec) Collect(ch chan<- prometheus.Metric) {
	for _, v := range c.values {
		ch <- v
	}

	c.values = nil
}

func (c *settableCounterVec) Set(value float64, labelValues ...string) {
	c.values = append(c.values, prometheus.MustNewConstMetric(c.desc, prometheus.CounterValue, value, labelValues...))
}

type settableCounter struct {
	desc  *prometheus.Desc
	value prometheus.Metric
}

func (c *settableCounter) Describe(ch chan<- *prometheus.Desc) {
	if c.desc == nil {
		log.WithField("counter", c).Warn("NIL description")
	}
	ch <- c.desc
}

func (c *settableCounter) Collect(ch chan<- prometheus.Metric) {
	if c.value == nil {
		log.WithField("counter", c).Warn("NIL value")
	}
	ch <- c.value
}

func (c *settableCounter) Set(value float64) {
	c.value = prometheus.MustNewConstMetric(c.desc, prometheus.CounterValue, value)
}

func newSettableCounter(subsystem, name, help string) *settableCounter {
	return &settableCounter{
		desc: prometheus.NewDesc(
			prometheus.BuildFQName("mesos", subsystem, name),
			help,
			nil,
			prometheus.Labels{},
		),
	}
}

func gauge(subsystem, name, help string, labels ...string) *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "mesos",
		Subsystem: subsystem,
		Name:      name,
		Help:      help,
	}, labels)
}

func counter(subsystem, name, help string, labels ...string) *settableCounterVec {
	desc := prometheus.NewDesc(
		prometheus.BuildFQName("mesos", subsystem, name),
		help,
		labels,
		prometheus.Labels{},
	)

	return &settableCounterVec{
		desc:   desc,
		values: nil,
	}
}

type authInfo struct {
	username      string
	password      string
	loginURL      string
	token         string
	tokenExpire   int64
	signingKey    []byte
	strictMode    bool
	privateKey    string
	skipSSLVerify bool
}

type httpClient struct {
	http.Client
	url       string
	auth      authInfo
	userAgent string
}

type versionCollector struct {
	*httpClient
	metric *prometheus.GaugeVec
}

func newVersionCollector(httpclient *httpClient) prometheus.Collector {
	// example data
	// "build_date": "2019-05-02 18:38:30",
	// "build_time": 1556822310,
	// "build_user": "foon",
	// "git_sha": "58cc918e9acc2865bb07047d3d2dff156d1708b2",
	// "git_tag": "1.7.2",
	// "version": "1.7.2"
	labels := []string{"build_date", "build_time", "git_sha", "git_tag", "version"}
	return &versionCollector{
		httpclient,
		gauge("", "version", "Version information for the mesos slave/master stored in labeling", labels...),
	}
}

type versionFields struct {
	BuildDate string  `json:"build_date,omitempty"`
	BuildTime float64 `json:"build_time,omitempty"`
	GitSHA    string  `json:"git_sha,omitempty"`
	GitTag    string  `json:"git_tag,omitempty"`
	Version   string  `json:"version,omitempty"`
}

func (v *versionCollector) Collect(ch chan<- prometheus.Metric) {
	var vf versionFields
	if v.fetchAndDecode("/version", &vf) {
		v.metric.WithLabelValues(vf.BuildDate, fmt.Sprintf("%f", vf.BuildTime), vf.GitSHA, vf.GitTag, vf.Version).Set(1)
		v.metric.Collect(ch)
	}
}

func (v *versionCollector) Describe(ch chan<- *prometheus.Desc) {
	v.metric.Describe(ch)
}

type metricCollector struct {
	*httpClient
	metrics map[prometheus.Collector]metricsCollectorFunctor
}

func newMetricCollector(httpClient *httpClient, metrics map[prometheus.Collector]metricsCollectorFunctor) prometheus.Collector {
	return &metricCollector{httpClient, metrics}
}

func signingToken(httpClient *httpClient) string {
	signKey, err := jwt.ParseRSAPrivateKeyFromPEM(httpClient.auth.signingKey)
	if err != nil {
		log.WithField("error", err).Error("Error parsing privateKey")
	}

	expireToken := time.Now().Add(time.Hour * 1).Unix()
	httpClient.auth.tokenExpire = expireToken

	// Create the token
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"uid": httpClient.auth.username,
		"exp": expireToken,
	})
	log.WithFields(log.Fields{
		"uid":     httpClient.auth.username,
		"expires": expireToken,
	}).Debug("creating token")
	// Sign and get the complete encoded token as a string
	tokenString, err := token.SignedString(signKey)
	if err != nil {
		log.WithField("error", err).Error("Error creating login token")
		return ""
	}
	return tokenString
}

func authToken(httpClient *httpClient) string {
	currentTime := time.Now().Unix()
	if currentTime > httpClient.auth.tokenExpire {
		url := httpClient.auth.loginURL
		signingToken := signingToken(httpClient)
		body, err := json.Marshal(&tokenRequest{UID: httpClient.auth.username, Token: signingToken})
		if err != nil {
			log.WithField("error", err).Error("Error creating JSON request")
			return ""
		}
		buffer := bytes.NewBuffer(body)
		req, err := http.NewRequest("POST", url, buffer)
		if err != nil {
			log.WithFields(log.Fields{
				"url":   url,
				"error": err,
			}).Error("Error creating HTTP request")
			return ""
		}
		req.Header.Add("User-Agent", httpClient.userAgent)
		req.Header.Add("Content-Type", "application/json")
		res, err := httpClient.Do(req)
		if err != nil {
			log.WithFields(log.Fields{
				"url":   url,
				"error": err,
			}).Error("Error fetching URL")
			errorCounter.Inc()
			return ""
		}
		defer res.Body.Close()

		var token tokenResponse
		if err := json.NewDecoder(res.Body).Decode(&token); err != nil {
			log.WithFields(log.Fields{
				"url":   url,
				"error": err,
			}).Error("Error decoding response body")
			errorCounter.Inc()
			return ""
		}

		httpClient.auth.token = fmt.Sprintf("token=%s", token.Token)
	}
	return httpClient.auth.token
}

func (httpClient *httpClient) fetchAndDecode(endpoint string, target interface{}) bool {
	url := strings.TrimSuffix(httpClient.url, "/") + endpoint
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.WithFields(log.Fields{
			"url":   url,
			"error": err,
		}).Error("Error creating HTTP request")
		return false
	}
	req.Header.Add("User-Agent", httpClient.userAgent)
	if httpClient.auth.username != "" && httpClient.auth.password != "" {
		req.SetBasicAuth(httpClient.auth.username, httpClient.auth.password)
	}
	if httpClient.auth.strictMode {
		req.Header.Add("Authorization", authToken(httpClient))
	}
	log.WithField("url", url).Debug("fetching URL")
	res, err := httpClient.Do(req)
	if err != nil {
		log.WithFields(log.Fields{
			"url":   url,
			"error": err,
		}).Error("Error fetching URL")
		errorCounter.Inc()
		return false
	}
	defer res.Body.Close()

	if err := json.NewDecoder(res.Body).Decode(&target); err != nil {
		log.WithFields(log.Fields{
			"url":   url,
			"error": err,
		}).Error("Error decoding response body")
		errorCounter.Inc()
		return false
	}

	return true
}

func (c *metricCollector) Collect(ch chan<- prometheus.Metric) {
	var m metricMap
	c.fetchAndDecode("/metrics/snapshot", &m)
	for cm, f := range c.metrics {
		if err := f(m, cm); err != nil {
			ch := make(chan *prometheus.Desc, 1)
			log.WithFields(log.Fields{
				"metric": <-ch,
				"error":  err,
			}).Error("Error extracting metric")
			errorCounter.Inc()
			continue
		}
		cm.Collect(ch)
	}
}

func (c *metricCollector) Describe(ch chan<- *prometheus.Desc) {
	for m := range c.metrics {
		m.Describe(ch)
	}
}

var invalidLabelNameCharRE = regexp.MustCompile("(^[^a-zA-Z_])|([^a-zA-Z0-9_])")

// Sanitize label names according to https://prometheus.io/docs/concepts/data_model/
func normaliseLabel(label string) string {
	return invalidLabelNameCharRE.ReplaceAllString(label, "_")
}

func normaliseLabelList(labelList []string) []string {
	normalisedLabelList := []string{}
	for _, label := range labelList {
		normalisedLabelList = append(normalisedLabelList, normaliseLabel(label))
	}
	return normalisedLabelList
}

func stringInSlice(string string, slice []string) bool {
	for _, elem := range slice {
		if string == elem {
			return true
		}
	}
	return false
}

func getLabelValuesFromMap(labels prometheus.Labels, orderedLabelKeys []string) []string {
	labelValues := []string{}
	for _, label := range orderedLabelKeys {
		labelValues = append(labelValues, labels[label])
	}
	return labelValues
}

var (
	text             = regexp.MustCompile("^[-[:word:]/.]*$")
	errDropAttribute = errors.New("value neither scalar nor text")
)

// attributeString converts a text attribute in json.RawMessage to string.
// see http://mesos.apache.org/documentation/latest/attributes-resources/
// for more information.  note that scalar matches text for this purpose.
// attributeString returns string or errDropAttribute.
func attributeString(attribute json.RawMessage) (string, error) {
	if value := strings.Trim(string(attribute), `"`); text.MatchString(value) {
		return value, nil
	}
	return "", errDropAttribute
}
