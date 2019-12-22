package main

//go:generate mockgen -source=provider.go -destination=provider_mock_test.go -package=main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"

	"github.com/hashicorp/terraform/addrs"
	"github.com/hashicorp/terraform/states"
	"github.com/sirupsen/logrus"
)

var (
	dryRun      bool
	logDebug    bool
	pathToState string
)

func init() {
	flag.BoolVar(&dryRun, "dry", false, "Don't delete anything")
	flag.BoolVar(&logDebug, "debug", false, "Enable debug logging")
	flag.StringVar(&pathToState, "state", "terraform.tfstate", "Path to a Terraform state file")
}

func main() {
	os.Exit(mainExitCode())
}

func mainExitCode() int {
	flag.Parse()

	provider := "aws"

	metaPlugin, tfDiagnostics, err := InstallProvider(provider, "2.43.0")
	if tfDiagnostics.HasErrors() {
		logrus.WithError(tfDiagnostics.Err()).Errorf("failed to install Terraform provider: %s", provider)
		return 1
	}
	if err != nil {
		logrus.WithError(err).Errorf("failed to install Terraform provider: %s", provider)
		return 1
	}

	// discard TRACE logs of GRPCProvider
	log.SetOutput(ioutil.Discard)

	logrus.SetFormatter(&logrus.TextFormatter{
		DisableTimestamp: true,
	})

	if logDebug {
		logrus.SetLevel(logrus.DebugLevel)
	}

	p, err := NewTerraformProvider(metaPlugin.Path, logDebug)
	if err != nil {
		logrus.WithError(err).Errorf("failed to load Terraform provider: %s", metaPlugin.Path)
		return 1
	}

	tfDiagnostics = p.Configure(awsProviderConfig())
	if tfDiagnostics.HasErrors() {
		logrus.WithError(tfDiagnostics.Err()).Fatal("failed to configure Terraform provider")
	}

	state, err := getState(pathToState)
	if err != nil {
		logrus.WithError(err).Errorf("failed to get Terraform state")
		return 1
	}
	logrus.Infof("using state: %s", pathToState)

	resInstances, diagnostics := lookupAllResourceInstanceAddrs(state)
	if diagnostics.HasErrors() {
		logrus.WithError(diagnostics.Err()).Errorf("failed to lookup resource instance addresses")
		return 1
	}

	deletedResourcesCount := 0

	for _, resAddr := range resInstances {
		logrus.Debugf("absolute address for resource instance (addr=%s)", resAddr.String())

		if resInstance := state.ResourceInstance(resAddr); resInstance.HasCurrent() {
			resMode := resAddr.Resource.Resource.Mode
			resType := resAddr.Resource.Resource.Type

			resID, err := getResourceID(resInstance)
			if err != nil {
				logrus.WithError(err).Errorf("failed to get ID for resource (addr=%s)", resAddr.String())
				return 1
			}

			logrus.Debugf("resource instance (mode=%s, type=%s, id=%s)", resMode, resType, resID)

			if resMode != addrs.ManagedResourceMode {
				logrus.Infof("can only delete managed resources defined by a resource block; therefore skipping resource (type=%s, id=%s)", resType, resID)
				continue
			}

			importResp := p.ImportResource(resType, resID)
			if importResp.Diagnostics.HasErrors() {
				logrus.WithError(importResp.Diagnostics.Err()).Infof("failed to import resource; therefore skipping resource (type=%s, id=%s)", resType, resID)
				continue
			}

			for _, resImp := range importResp.ImportedResources {
				logrus.Debugf("imported resource (type=%s, id=%s): %s", resType, resID, resImp.State.GoString())

				readResp := p.ReadResource(resImp)
				if readResp.Diagnostics.HasErrors() {
					logrus.WithError(readResp.Diagnostics.Err()).Infof("failed to read resource and refreshing its current state; therefore skipping resource (type=%s, id=%s)", resType, resID)
					continue
				}

				logrus.Debugf("read resource (type=%s, id=%s): %s", resType, resID, readResp.NewState.GoString())

				resourceNotExists := readResp.NewState.IsNull()
				if resourceNotExists {
					logrus.Infof("resource found in state does not exist anymore (type=%s, id=%s)", resImp.TypeName, resID)
					continue
				}

				if p.DeleteResource(resType, resID, readResp, dryRun) {
					deletedResourcesCount++
				}
			}
		}
	}

	logrus.Infof("total number of resources deleted: %d\n", deletedResourcesCount)

	return 0
}

func getResourceID(resInstance *states.ResourceInstance) (string, error) {
	var result ResourceID

	err := json.Unmarshal(resInstance.Current.AttrsJSON, &result)
	if err != nil {
		return "", fmt.Errorf("failed to unmarshal JSON-encoded resource instance attributes: %s", err)
	}

	logrus.Debugf("resource instance attributes: %s", resInstance.Current.AttrsJSON)

	return result.ID, nil
}

type ResourceID struct {
	ID string `json:"id"`
}
