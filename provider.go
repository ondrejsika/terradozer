package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"

	"github.com/apex/log"
	"github.com/hashicorp/go-hclog"
	goPlugin "github.com/hashicorp/go-plugin"
	"github.com/hashicorp/go-version"
	"github.com/hashicorp/terraform/addrs"
	"github.com/hashicorp/terraform/plugin"
	"github.com/hashicorp/terraform/plugin/discovery"
	"github.com/hashicorp/terraform/providers"
	"github.com/hashicorp/terraform/tfdiags"
	"github.com/mitchellh/cli"
	"github.com/zclconf/go-cty/cty"
)

// Provider is the interface that every Terraform Provider Plugin implements
type Provider interface {
	Configure(providers.ConfigureRequest) providers.ConfigureResponse
	ReadResource(providers.ReadResourceRequest) providers.ReadResourceResponse
	ApplyResourceChange(providers.ApplyResourceChangeRequest) providers.ApplyResourceChangeResponse
	ImportResourceState(providers.ImportResourceStateRequest) providers.ImportResourceStateResponse
}

type TerraformProvider struct {
	provider Provider
}

func newTerraformProvider(path string, logDebug bool) (*TerraformProvider, error) {
	m := discovery.PluginMeta{
		Path: path,
	}

	hcLoglevel := hclog.Error
	if logDebug {
		hcLoglevel = hclog.Debug
	}

	p, err := providerFactory(m, hcLoglevel)()
	if err != nil {
		return nil, err
	}

	return &TerraformProvider{p}, nil
}

// copied (and modified) from github.com/hashicorp/terraform/command/plugins.go
func providerFactory(meta discovery.PluginMeta, loglevel hclog.Level) providers.Factory {
	return func() (providers.Interface, error) {
		client := goPlugin.NewClient(clientConfig(meta, loglevel))
		// Request the RPC client so we can get the provider
		// so we can build the actual RPC-implemented provider.
		rpcClient, err := client.Client()
		if err != nil {
			return nil, err
		}

		raw, err := rpcClient.Dispense(plugin.ProviderPluginName)
		if err != nil {
			return nil, err
		}

		// store the client so that the plugin can kill the child process
		p := raw.(*plugin.GRPCProvider)
		p.PluginClient = client
		return p, nil
	}
}

// copied (and modified) from terraform/plugin/client.go
func clientConfig(m discovery.PluginMeta, loglevel hclog.Level) *goPlugin.ClientConfig {
	logger := hclog.New(&hclog.LoggerOptions{
		Name:   "plugin",
		Level:  loglevel,
		Output: os.Stderr,
	})

	return &goPlugin.ClientConfig{
		Cmd:              exec.Command(m.Path), //nolint:gosec
		HandshakeConfig:  plugin.Handshake,
		VersionedPlugins: plugin.VersionedPlugins,
		Managed:          true,
		Logger:           logger,
		AllowedProtocols: []goPlugin.Protocol{goPlugin.ProtocolGRPC},
		AutoMTLS:         true,
	}
}

func (p TerraformProvider) configure(config cty.Value) tfdiags.Diagnostics {
	respConf := p.provider.Configure(providers.ConfigureRequest{
		Config: config,
	})

	return respConf.Diagnostics
}

func (p TerraformProvider) importResource(resType string, resID string) providers.ImportResourceStateResponse {
	response := p.provider.ImportResourceState(providers.ImportResourceStateRequest{
		TypeName: resType,
		ID:       resID,
	})

	return response
}

func (p TerraformProvider) readResource(r providers.ImportedResource) providers.ReadResourceResponse {
	response := p.provider.ReadResource(providers.ReadResourceRequest{
		TypeName:   r.TypeName,
		PriorState: r.State,
		Private:    r.Private,
	})

	return response
}

func (p TerraformProvider) destroy(resType string, currentState cty.Value) providers.ApplyResourceChangeResponse {
	response := p.provider.ApplyResourceChange(providers.ApplyResourceChangeRequest{
		TypeName:     resType,
		PriorState:   enableForceDestroyAttributes(currentState),
		PlannedState: cty.NullVal(cty.DynamicPseudoType),
		Config:       cty.NullVal(cty.DynamicPseudoType),
	})

	return response
}

// enableForceDestroyAttributes sets force destroy attributes of a resource to true
// to be able to successfully delete some resources
// (eg. a non-empty S3 bucket or a AWS IAM role with attached policies).
//
// Note: this is at the moment AWS specific
func enableForceDestroyAttributes(state cty.Value) cty.Value {
	stateWithDestroyAttrs := map[string]cty.Value{}

	if state.CanIterateElements() {
		for k, v := range state.AsValueMap() {
			if k == "force_detach_policies" || k == "force_destroy" {
				if v.Type().Equals(cty.Bool) {
					stateWithDestroyAttrs[k] = cty.True
				}
			} else {
				stateWithDestroyAttrs[k] = v
			}
		}
	}

	return cty.ObjectVal(stateWithDestroyAttrs)
}

// installProvider downloads the provider plugin binary
func installProvider(providerName, constraint string, useCache bool) (discovery.PluginMeta, error) {
	installDir := ".terradozer"

	providerInstaller := &discovery.ProviderInstaller{
		Dir: installDir,
		Cache: func() discovery.PluginCache {
			if useCache {
				return discovery.NewLocalPluginCache(installDir + "/cache")
			}
			return nil
		}(),
		PluginProtocolVersion: discovery.PluginInstallProtocolVersion,
		SkipVerify:            false,
		Ui: &cli.BasicUi{
			Reader:      os.Stdin,
			Writer:      &bytes.Buffer{},
			ErrorWriter: os.Stderr,
		},
	}

	providerConstraint := discovery.AllVersions

	if constraint != "" {
		constraints, err := version.NewConstraint(constraint)
		if err != nil {
			return discovery.PluginMeta{}, fmt.Errorf("failed to parse provider version constraint: %s", err)
		}

		providerConstraint = discovery.NewConstraints(constraints)
	}

	pty := addrs.NewLegacyProvider(providerName)

	meta, tfDiagnostics, err := providerInstaller.Get(pty, providerConstraint)
	if err != nil {
		tfDiagnostics = tfDiagnostics.Append(err)
		return discovery.PluginMeta{}, tfDiagnostics.Err()
	}

	return meta, nil
}

// InitProviders installs, initializes (starts the plugin binary process), and configures
// each provider in the given list of provider names
func InitProviders(providerNames []string) (map[string]*TerraformProvider, error) {
	providers := map[string]*TerraformProvider{}

	for _, pName := range providerNames {
		log.WithField("name", pName).Debug(Pad("starting to initialize provider"))

		pConfig, pVersion, err := ProviderConfig(pName)
		if err != nil {
			log.WithField("name", pName).Info(Pad("ignoring resources of (yet) unsupported provider"))
			continue
		}

		metaPlugin, err := installProvider(pName, pVersion, true)
		if err != nil {
			return nil, fmt.Errorf("failed to install provider (%s): %s", pName, err)
		}

		log.WithFields(log.Fields{
			"name":    metaPlugin.Name,
			"version": metaPlugin.Version,
		}).Info(Pad("downloaded and installed provider"))

		p, err := newTerraformProvider(metaPlugin.Path, logDebug)
		if err != nil {
			return nil, fmt.Errorf("failed to load Terraform provider (%s): %s", metaPlugin.Path, err)
		}

		tfDiagnostics := p.configure(pConfig)
		if tfDiagnostics.HasErrors() {
			return nil, fmt.Errorf("failed to configure provider (name=%s, version=%s): %s",
				metaPlugin.Name, metaPlugin.Version, tfDiagnostics.Err())
		}

		log.WithFields(log.Fields{
			"name":    metaPlugin.Name,
			"version": metaPlugin.Version,
		}).Info(Pad("configured provider"))

		providers[pName] = p
	}

	return providers, nil
}
