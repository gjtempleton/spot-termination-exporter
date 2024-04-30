package main

import (
	"encoding/json"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	"net/http"
	"time"
)

type terminationCollector struct {
	metadataEndpoint          string
	tokenEndpoint             string
	useIMDSv2                 bool
	rebalanceIndicator        *prometheus.Desc
	rebalanceScrapeSuccessful *prometheus.Desc
	scrapeSuccessful          *prometheus.Desc
	terminationIndicator      *prometheus.Desc
	terminationTime           *prometheus.Desc
}

type instanceAction struct {
	Action string    `json:"action"`
	Time   time.Time `json:"time"`
}

type instanceEvent struct {
	NoticeTime time.Time `json:"noticeTime"`
}

func NewTerminationCollector(metadataEndpoint, tokenEndpoint string, useIMDSv2 bool) *terminationCollector {
	return &terminationCollector{
		metadataEndpoint:          metadataEndpoint,
		tokenEndpoint:             tokenEndpoint,
		useIMDSv2:                 useIMDSv2,
		rebalanceIndicator:        prometheus.NewDesc("aws_instance_rebalance_recommended", "Instance rebalance is recommended", []string{"instance_id", "instance_type"}, nil),
		rebalanceScrapeSuccessful: prometheus.NewDesc("aws_instance_metadata_service_events_available", "Metadata service events endpoint available", []string{"instance_id"}, nil),
		scrapeSuccessful:          prometheus.NewDesc("aws_instance_metadata_service_available", "Metadata service available", []string{"instance_id"}, nil),
		terminationIndicator:      prometheus.NewDesc("aws_instance_termination_imminent", "Instance is about to be terminated", []string{"instance_action", "instance_id", "instance_type"}, nil),
		terminationTime:           prometheus.NewDesc("aws_instance_termination_in", "Instance will be terminated in", []string{"instance_id", "instance_type"}, nil),
	}
}

func (c *terminationCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.rebalanceIndicator
	ch <- c.rebalanceScrapeSuccessful
	ch <- c.scrapeSuccessful
	ch <- c.terminationIndicator
	ch <- c.terminationTime

}

func (c *terminationCollector) Collect(ch chan<- prometheus.Metric) {
	log.Info("Fetching termination data from metadata-service")

	timeout := time.Duration(1 * time.Second)
	client := http.Client{
		Timeout: timeout,
	}

	token := ""
	if c.useIMDSv2 {
		maybeToken, err := c.getIMDSv2Token(&client, c.tokenEndpoint)
		if err != nil {
			log.Errorf("couldn't fetch token for IMDSv2: %s", err.Error())
			return
		}
		token = maybeToken
	}

	idResp, err := c.getResponse(&client, c.metadataEndpoint+"instance-id", token)
	var instanceID string
	if err != nil {
		log.Errorf("couldn't parse instance-id from metadata: %s", err.Error())
		return
	}
	if idResp.StatusCode == 404 {
		log.Errorf("couldn't parse instance-id from metadata: endpoint not found")
		return
	}
	defer idResp.Body.Close()
	body, _ := ioutil.ReadAll(idResp.Body)
	instanceID = string(body)

	typeResp, err := c.getResponse(&client, c.metadataEndpoint+"instance-type", token)
	var instanceType string
	if err != nil {
		log.Errorf("couldn't parse instance-type from metadata: %s", err.Error())
		return
	}
	if typeResp.StatusCode == 404 {
		log.Errorf("couldn't parse instance-type from metadata: endpoint not found")
		return
	}
	defer typeResp.Body.Close()
	body, _ = ioutil.ReadAll(typeResp.Body)
	instanceType = string(body)

	resp, err := c.getResponse(&client, c.metadataEndpoint+"spot/instance-action", token)
	if err != nil {
		log.Errorf("Failed to fetch data from metadata service: %s", err)
		ch <- prometheus.MustNewConstMetric(c.scrapeSuccessful, prometheus.GaugeValue, 0, instanceID)
	} else {
		ch <- prometheus.MustNewConstMetric(c.scrapeSuccessful, prometheus.GaugeValue, 1, instanceID)

		if resp.StatusCode == 404 {
			log.Debug("instance-action endpoint not found")
			ch <- prometheus.MustNewConstMetric(c.terminationIndicator, prometheus.GaugeValue, 0, "", instanceID, instanceType)
		} else {
			defer resp.Body.Close()
			body, _ := ioutil.ReadAll(resp.Body)

			var ia = instanceAction{}
			err := json.Unmarshal(body, &ia)

			// value may be present but not be a time according to AWS docs,
			// so parse error is not fatal
			if err != nil {
				log.Errorf("Couldn't parse instance-action metadata: %s", err)
				ch <- prometheus.MustNewConstMetric(c.terminationIndicator, prometheus.GaugeValue, 0, instanceID, instanceType)
			} else {
				log.Infof("instance-action endpoint available, termination time: %v", ia.Time)
				ch <- prometheus.MustNewConstMetric(c.terminationIndicator, prometheus.GaugeValue, 1, ia.Action, instanceID, instanceType)
				delta := ia.Time.Sub(time.Now())
				if delta.Seconds() > 0 {
					ch <- prometheus.MustNewConstMetric(c.terminationTime, prometheus.GaugeValue, delta.Seconds(), instanceID, instanceType)
				}
			}
		}
	}

	eventResp, err := c.getResponse(&client, c.metadataEndpoint+"events/recommendations/rebalance", token)
	if err != nil {
		log.Errorf("Failed to fetch events data from metadata service: %s", err)
		ch <- prometheus.MustNewConstMetric(c.rebalanceScrapeSuccessful, prometheus.GaugeValue, 0, instanceID)
		// Return early as this is the last metric/metadata scrape attempt
		return
	} else {
		ch <- prometheus.MustNewConstMetric(c.rebalanceScrapeSuccessful, prometheus.GaugeValue, 1, instanceID)

		if eventResp.StatusCode == 404 {
			log.Debug("rebalance endpoint not found")
			ch <- prometheus.MustNewConstMetric(c.rebalanceIndicator, prometheus.GaugeValue, 0, instanceID, instanceType)
			// Return early as this is the last metric/metadata scrape attempt
			return
		} else {
			defer eventResp.Body.Close()
			body, _ := ioutil.ReadAll(eventResp.Body)

			var ie = instanceEvent{}
			err := json.Unmarshal(body, &ie)

			if err != nil {
				log.Errorf("Couldn't parse rebalance recommendation event metadata: %s", err)
				ch <- prometheus.MustNewConstMetric(c.rebalanceIndicator, prometheus.GaugeValue, 0, instanceID, instanceType)
			} else {
				log.Infof("rebalance recommendation event endpoint available, recommendation time: %v", ie.NoticeTime)
				ch <- prometheus.MustNewConstMetric(c.rebalanceIndicator, prometheus.GaugeValue, 1, instanceID, instanceType)
			}
		}
	}
}

func (c *terminationCollector) getIMDSv2Token(client *http.Client, url string) (string, error) {
	req, err := http.NewRequest("PUT", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Add("X-aws-ec2-metadata-token-ttl-seconds", "21600")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func (c *terminationCollector) getResponse(client *http.Client, url, token string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Add("X-aws-ec2-metadata-token", token)
	}
	return client.Do(req)
}
