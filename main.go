package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/types"

	providerTypes "github.com/openfaas/faas-provider/types"
	"github.com/openfaas/faas/gateway/metrics"
	"github.com/openfaas/faas/gateway/requests"
)

const scaleLabel = "com.openfaas.scale.zero"

var dryRun bool

var writeDebug bool

type Credentials struct {
	Username string
	Password string
}

func main() {
	config, configErr := types.ReadConfig()
	if configErr != nil {
		log.Panic(configErr.Error())
		os.Exit(1)
	}

	flag.BoolVar(&dryRun, "dry-run", false, "use dry-run for scaling events")
	flag.Parse()

	if val, ok := os.LookupEnv("write_debug"); ok && (val == "1" || val == "true") {
		writeDebug = true
	}

	credentials := Credentials{}

	val, err := readFile("/var/secrets/basic-auth-user")
	if err == nil {
		credentials.Username = val
	} else {
		log.Printf("Unable to read username: %s", err)
	}

	passwordVal, passErr := readFile("/var/secrets/basic-auth-password")
	if passErr == nil {
		credentials.Password = passwordVal
	} else {
		log.Printf("Unable to read password: %s", err)
	}

	client := &http.Client{}
	version, err := getVersion(client, config.GatewayURL, &credentials)

	if err != nil {
		panic(err)
	}

	log.Printf("Gateway version: %s, SHA: %s\n", version.Version.Release, version.Version.SHA)

	fmt.Printf(`dry_run: %t
gateway_url: %s
inactivity_duration: %s `, dryRun, config.GatewayURL, config.InactivityDuration)

	if len(config.GatewayURL) == 0 {
		fmt.Println("gateway_url (faas-netes/faas-swarm) is required.")
		os.Exit(1)
	}

	for {
		reconcile(client, config, &credentials)
		time.Sleep(config.ReconcileInterval)
		fmt.Printf("\n")
	}
}

func readFile(path string) (string, error) {
	if _, err := os.Stat(path); err == nil {
		data, readErr := ioutil.ReadFile(path)
		return strings.TrimSpace(string(data)), readErr
	}
	return "", nil
}

func buildMetricsMap(client *http.Client, functions []requests.Function, config types.Config) map[string]float64 {
	query := metrics.NewPrometheusQuery(config.PrometheusHost, config.PrometheusPort, client)
	metrics := make(map[string]float64)

	duration := fmt.Sprintf("%dm", int(config.InactivityDuration.Minutes()))
	// duration := "5m"

	for _, function := range functions {
		querySt := url.QueryEscape(`sum(rate(gateway_function_invocation_total{function_name="` + function.Name + `", code=~".*"}[` + duration + `])) by (code, function_name)`)

		res, err := query.Fetch(querySt)
		if err != nil {
			log.Println(err)
			continue
		}

		if len(res.Data.Result) > 0 || function.InvocationCount == 0 {

			if _, exists := metrics[function.Name]; !exists {
				metrics[function.Name] = 0
			}

			for _, v := range res.Data.Result {

				if writeDebug {
					fmt.Println(v)
				}

				if v.Metric.FunctionName == function.Name {
					metricValue := v.Value[1]
					switch metricValue.(type) {
					case string:

						f, strconvErr := strconv.ParseFloat(metricValue.(string), 64)
						if strconvErr != nil {
							log.Printf("Unable to convert value for metric: %s\n", strconvErr)
							continue
						}

						metrics[function.Name] = metrics[function.Name] + f
					}
				}
			}
		}
	}
	return metrics
}

func reconcile(client *http.Client, config types.Config, credentials *Credentials) {
	functions, err := queryFunctions(client, config.GatewayURL, credentials)

	if err != nil {
		log.Println(err)
		return
	}

	metrics := buildMetricsMap(client, functions, config)

	for _, fn := range functions {

		if fn.Labels != nil {
			labels := *fn.Labels
			labelValue := labels[scaleLabel]

			if labelValue != "1" && labelValue != "true" {
				if writeDebug {
					log.Printf("Skip: %s due to missing label\n", fn.Name)
				}
				continue
			}
		}

		if v, found := metrics[fn.Name]; found {
			if v == float64(0) {
				fmt.Printf("%s\tidle\n", fn.Name)

				if val, _ := getReplicas(client, config.GatewayURL, fn.Name, credentials); val != nil && val.AvailableReplicas > 0 {
					sendScaleEvent(client, config.GatewayURL, fn.Name, uint64(0), credentials)
				}

			} else {
				if writeDebug {
					fmt.Printf("%s\tactive: %f\n", fn.Name, v)
				}
			}
		}
	}
}

func getReplicas(client *http.Client, gatewayURL string, name string, credentials *Credentials) (*requests.Function, error) {
	item := &requests.Function{}
	var err error

	req, _ := http.NewRequest(http.MethodGet, gatewayURL+"system/function/"+name, nil)
	req.SetBasicAuth(credentials.Username, credentials.Password)

	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	if res.Body != nil {
		defer res.Body.Close()
	}

	bytesOut, _ := ioutil.ReadAll(res.Body)

	err = json.Unmarshal(bytesOut, &item)

	return item, err
}

func queryFunctions(client *http.Client, gatewayURL string, credentials *Credentials) ([]requests.Function, error) {
	list := []requests.Function{}
	var err error

	req, _ := http.NewRequest(http.MethodGet, gatewayURL+"system/functions", nil)
	req.SetBasicAuth(credentials.Username, credentials.Password)

	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	if res.Body != nil {
		defer res.Body.Close()
	}

	bytesOut, _ := ioutil.ReadAll(res.Body)

	err = json.Unmarshal(bytesOut, &list)

	return list, err
}

func sendScaleEvent(client *http.Client, gatewayURL string, name string, replicas uint64, credentials *Credentials) {
	if dryRun {
		fmt.Printf("dry-run: Scaling %s to %d replicas\n", name, replicas)
		return
	}

	scaleReq := providerTypes.ScaleServiceRequest{
		ServiceName: name,
		Replicas:    replicas,
	}

	var err error

	bodyBytes, _ := json.Marshal(scaleReq)
	bodyReader := bytes.NewReader(bodyBytes)

	req, _ := http.NewRequest(http.MethodPost, gatewayURL+"system/scale-function/"+name, bodyReader)
	req.SetBasicAuth(credentials.Username, credentials.Password)

	res, err := client.Do(req)

	if err != nil {
		log.Println(err)
		return
	}
	log.Println("Scale", name, res.StatusCode, replicas)

	if res.Body != nil {
		defer res.Body.Close()
	}
}

// Version holds the GitHub Release and SHA
type Version struct {
	Version struct {
		Release string `json:"release"`
		SHA     string `json:"sha"`
	}
}

func getVersion(client *http.Client, gatewayURL string, credentials *Credentials) (Version, error) {
	version := Version{}
	var err error

	req, _ := http.NewRequest(http.MethodGet, gatewayURL+"system/info", nil)
	req.SetBasicAuth(credentials.Username, credentials.Password)

	res, err := client.Do(req)
	if err != nil {
		return version, err
	}

	if res.Body != nil {
		defer res.Body.Close()
	}

	bytesOut, _ := ioutil.ReadAll(res.Body)

	err = json.Unmarshal(bytesOut, &version)

	return version, err
}
