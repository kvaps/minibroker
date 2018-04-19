package minibroker

import (
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/Masterminds/semver"
	"github.com/davecgh/go-spew/spew"
	"github.com/golang/glog"
	"github.com/osbkit/minibroker/pkg/helm"
	"github.com/osbkit/minibroker/pkg/tiller"
	"github.com/pkg/errors"
	osb "github.com/pmorie/go-open-service-broker-client/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/helm/pkg/repo"
)

type Client struct {
	helm       *helm.Client
	coreClient kubernetes.Interface
	instances  map[string]*exampleInstance
}

func NewClient(repoURL string) *Client {
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	return &Client{
		helm:       helm.NewClient(repoURL),
		coreClient: clientset,
		instances:  make(map[string]*exampleInstance, 10),
	}
}

func (c *Client) Init() error {
	return c.helm.Init()
}

func (c *Client) ListServices() ([]osb.Service, error) {
	glog.Info("Listing services...")
	var services []osb.Service

	charts, err := c.helm.ListCharts()
	if err != nil {
		return nil, err
	}

	for chart, chartVersions := range charts {
		svc := osb.Service{
			ID:          chart,
			Name:        chart,
			Description: "Helm Chart for " + chart,
			Bindable:    true,
			Plans:       make([]osb.Plan, 0, len(chartVersions)),
		}
		appVersions := map[string]*repo.ChartVersion{}
		for _, chartVersion := range chartVersions {
			if chartVersion.AppVersion == "" {
				continue
			}

			curV, err := semver.NewVersion(chartVersion.Version)
			if err != nil {
				fmt.Printf("Skipping %s@%s because %s is not a valid semver", chart, chartVersion.AppVersion, chartVersion.Version)
				continue
			}

			currentMax, ok := appVersions[chartVersion.AppVersion]
			if !ok {
				appVersions[chartVersion.AppVersion] = chartVersion
			} else {
				maxV, _ := semver.NewVersion(currentMax.Version)
				if curV.GreaterThan(maxV) {
					appVersions[chartVersion.AppVersion] = chartVersion
				} else {
					//fmt.Printf("Skipping %s@%s because %s<%s\n", chart, chartVersion.AppVersion, curV, maxV)
					continue
				}
			}
		}

		for _, chartVersion := range appVersions {
			planToken := fmt.Sprintf("%s@%s", chart, chartVersion.AppVersion)
			cleaner := regexp.MustCompile(`[^a-z0-9]`)
			planID := cleaner.ReplaceAllString(strings.ToLower(planToken), "-")
			planName := cleaner.ReplaceAllString(chartVersion.AppVersion, "-")
			plan := osb.Plan{
				ID:          planID,
				Name:        planName,
				Description: chartVersion.Description,
				Free:        boolPtr(true),
			}
			svc.Plans = append(svc.Plans, plan)
		}

		if len(svc.Plans) == 0 {
			continue
		}
		services = append(services, svc)
	}

	glog.Infoln("List complete")
	return services, nil
}

func (c *Client) Provision(instanceID, serviceID, planID, namespace string) error {
	chartName := serviceID
	// TODO: The way I'm turning charts into plans is not reversible. Need a data store.
	chartVersion := strings.Replace(planID, serviceID+"-", "", 1)
	chartVersion = strings.Replace(chartVersion, "-", ".", -1)

	glog.Infof("Provisioning %s/%s using stable helm chart %s@%s...", serviceID, planID, chartName, chartVersion)

	chartDef, err := c.helm.GetChart(chartName, chartVersion)
	if err != nil {
		return err
	}

	config := tiller.Config{
		Host: "localhost",
		Port: 44134,
	}
	tc, err := config.NewClient()
	if err != nil {
		return err
	}
	defer func() {
		err := tc.Close()
		if err != nil {
			log.Print(errors.Wrapf(err, "failed to disconnect tiller client"))
		}
	}()

	chart, err := helm.LoadChart(chartDef)
	if err != nil {
		return err
	}

	resp, err := tc.Create(chart, namespace)
	if err != nil {
		return err
	}

	glog.Infof("Provision of %v@%v (%v@%v) complete\n%s\n",
		chartName, chartVersion, resp.Release.Name, resp.Release.Version, spew.Sdump(resp.Release.Manifest))
	c.instances[instanceID] = &exampleInstance{
		ID:        instanceID,
		Release:   resp.Release.Name,
		Namespace: namespace,
	}

	return nil
}

func (c *Client) Bind(instaneID string) (map[string]interface{}, error) {
	instance, ok := c.instances[instaneID]
	if !ok {
		return nil, osb.HTTPStatusCodeError{
			StatusCode: http.StatusNotFound,
		}
	}

	opts := metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{
			"heritage": "Tiller",
			"release":  instance.Release,
		}).String(),
	}
	secrets, err := c.coreClient.CoreV1().Secrets(instance.Namespace).List(opts)
	if err != nil {
		return nil, err
	}

	creds := make(map[string]interface{})
	for _, secret := range secrets.Items {
		for key, value := range secret.Data {
			creds[key] = string(value)
		}
	}

	return creds, nil
}

func boolPtr(value bool) *bool {
	return &value
}