// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package spec

import (
	"context"
	"crypto/tls"
	"fmt"
	"path/filepath"
	"time"

	perrs "github.com/pingcap/errors"
	"github.com/pingcap/tiup/pkg/cluster/api"
	"github.com/pingcap/tiup/pkg/cluster/ctxt"
	"github.com/pingcap/tiup/pkg/cluster/template/scripts"
	"github.com/pingcap/tiup/pkg/meta"
	"github.com/pingcap/tiup/pkg/tidbver"
	"github.com/pingcap/tiup/pkg/utils"
)

// CDCSpec represents the Drainer topology specification in topology.yaml
type CDCSpec struct {
	Host            string                 `yaml:"host"`
	SSHPort         int                    `yaml:"ssh_port,omitempty" validate:"ssh_port:editable"`
	Imported        bool                   `yaml:"imported,omitempty"`
	Patched         bool                   `yaml:"patched,omitempty"`
	IgnoreExporter  bool                   `yaml:"ignore_exporter,omitempty"`
	Port            int                    `yaml:"port" default:"8300"`
	DeployDir       string                 `yaml:"deploy_dir,omitempty"`
	DataDir         string                 `yaml:"data_dir,omitempty"`
	LogDir          string                 `yaml:"log_dir,omitempty"`
	Offline         bool                   `yaml:"offline,omitempty"`
	GCTTL           int64                  `yaml:"gc-ttl,omitempty" validate:"gc-ttl:editable"`
	TZ              string                 `yaml:"tz,omitempty" validate:"tz:editable"`
	NumaNode        string                 `yaml:"numa_node,omitempty" validate:"numa_node:editable"`
	Config          map[string]interface{} `yaml:"config,omitempty" validate:"config:ignore"`
	ResourceControl meta.ResourceControl   `yaml:"resource_control,omitempty" validate:"resource_control:editable"`
	Arch            string                 `yaml:"arch,omitempty"`
	OS              string                 `yaml:"os,omitempty"`
}

// Role returns the component role of the instance
func (s *CDCSpec) Role() string {
	return ComponentCDC
}

// SSH returns the host and SSH port of the instance
func (s *CDCSpec) SSH() (string, int) {
	return s.Host, s.SSHPort
}

// GetMainPort returns the main port of the instance
func (s *CDCSpec) GetMainPort() int {
	return s.Port
}

// IsImported returns if the node is imported from TiDB-Ansible
func (s *CDCSpec) IsImported() bool {
	return s.Imported
}

// IgnoreMonitorAgent returns if the node does not have monitor agents available
func (s *CDCSpec) IgnoreMonitorAgent() bool {
	return s.IgnoreExporter
}

// CDCComponent represents CDC component.
type CDCComponent struct{ Topology *Specification }

// Name implements Component interface.
func (c *CDCComponent) Name() string {
	return ComponentCDC
}

// Role implements Component interface.
func (c *CDCComponent) Role() string {
	return ComponentCDC
}

// Instances implements Component interface.
func (c *CDCComponent) Instances() []Instance {
	ins := make([]Instance, 0, len(c.Topology.CDCServers))
	for _, s := range c.Topology.CDCServers {
		s := s
		instance := &CDCInstance{BaseInstance{
			InstanceSpec: s,
			Name:         c.Name(),
			Host:         s.Host,
			Port:         s.Port,
			SSHP:         s.SSHPort,

			Ports: []int{
				s.Port,
			},
			Dirs: []string{
				s.DeployDir,
			},
			StatusFn: func(_ context.Context, timeout time.Duration, tlsCfg *tls.Config, _ ...string) string {
				return statusByHost(s.Host, s.Port, "/status", timeout, tlsCfg)
			},
			UptimeFn: func(_ context.Context, timeout time.Duration, tlsCfg *tls.Config) time.Duration {
				return UptimeByHost(s.Host, s.Port, timeout, tlsCfg)
			},
		}, c.Topology}
		if s.DataDir != "" {
			instance.Dirs = append(instance.Dirs, s.DataDir)
		}

		ins = append(ins, instance)
	}
	return ins
}

// CDCInstance represent the CDC instance.
type CDCInstance struct {
	BaseInstance
	topo Topology
}

// ScaleConfig deploy temporary config on scaling
func (i *CDCInstance) ScaleConfig(
	ctx context.Context,
	e ctxt.Executor,
	topo Topology,
	clusterName,
	clusterVersion,
	user string,
	paths meta.DirPaths,
) error {
	s := i.topo
	defer func() {
		i.topo = s
	}()
	i.topo = mustBeClusterTopo(topo)

	return i.InitConfig(ctx, e, clusterName, clusterVersion, user, paths)
}

// InitConfig implements Instance interface.
func (i *CDCInstance) InitConfig(
	ctx context.Context,
	e ctxt.Executor,
	clusterName,
	clusterVersion,
	deployUser string,
	paths meta.DirPaths,
) error {
	topo := i.topo.(*Specification)
	if err := i.BaseInstance.InitConfig(ctx, e, topo.GlobalOptions, deployUser, paths); err != nil {
		return err
	}
	enableTLS := topo.GlobalOptions.TLSEnabled
	spec := i.InstanceSpec.(*CDCSpec)
	globalConfig := topo.ServerConfigs.CDC
	instanceConfig := spec.Config

	if !tidbver.TiCDCSupportConfigFile(clusterVersion) {
		if len(globalConfig)+len(instanceConfig) > 0 {
			return perrs.New("server_config is only supported with TiCDC version v4.0.13 or later")
		}
	}

	cfg := scripts.NewCDCScript(
		i.GetHost(),
		paths.Deploy,
		paths.Log,
		enableTLS,
		spec.GCTTL,
		spec.TZ,
	).WithPort(spec.Port).WithNumaNode(spec.NumaNode).AppendEndpoints(topo.Endpoints(deployUser)...)

	// doesn't work
	if _, err := i.setTLSConfig(ctx, false, nil, paths); err != nil {
		return err
	}
	if len(paths.Data) != 0 {
		cfg = cfg.PatchByVersion(clusterVersion, paths.Data[0])
	}

	fp := filepath.Join(paths.Cache, fmt.Sprintf("run_cdc_%s_%d.sh", i.GetHost(), i.GetPort()))

	if err := cfg.ConfigToFile(fp); err != nil {
		return err
	}
	dst := filepath.Join(paths.Deploy, "scripts", "run_cdc.sh")
	if err := e.Transfer(ctx, fp, dst, false, 0, false); err != nil {
		return err
	}

	if _, _, err := e.Execute(ctx, "chmod +x "+dst, false); err != nil {
		return err
	}

	return i.MergeServerConfig(ctx, e, globalConfig, instanceConfig, paths)
}

// setTLSConfig set TLS Config to support enable/disable TLS
func (i *CDCInstance) setTLSConfig(ctx context.Context, enableTLS bool, configs map[string]interface{}, paths meta.DirPaths) (map[string]interface{}, error) {
	return nil, nil
}

var _ RollingUpdateInstance = &CDCInstance{}

// PreRestart implements RollingUpdateInstance interface.
func (i *CDCInstance) PreRestart(ctx context.Context, topo Topology, apiTimeoutSeconds int, tlsCfg *tls.Config) error {
	tidbTopo, ok := topo.(*Specification)
	if !ok {
		panic("should be type of tidb topology")
	}

	// cdc rolling upgrade strategy only works if there are more than 2 captures
	// todo: shall we detect server count by request the cdc cluster instead of the specification.
	if len(tidbTopo.CDCServers) <= 1 {
		return nil
	}

	timeoutOpt := &utils.RetryOption{
		Timeout: time.Second * time.Duration(apiTimeoutSeconds),
		Delay:   time.Second * 2,
	}

	client := api.NewCDCOpenAPIClient(tidbTopo.GetCDCList(), 5*time.Second, tlsCfg)

	var captureID string
	count, err := client.DrainCapture(captureID)
	if err != nil {
		return err
	}

	// retry here, until count become 0
	for count != 0 {
		// send request to the cdc in a proper way.
	}

	if count != 0 {

	}

	return nil
}

//func (i *CDCInstance) drainCapture() error {
//	return nil
//}

// PostRestart implements RollingUpdateInstance interface.
func (i *CDCInstance) PostRestart(ctx context.Context, topo Topology, tlsCfg *tls.Config) error {
	return nil
}

// IsOwner return true if the instance is the TiCDC owner
// todo: reset this apiTimeout to a much more reasonable value
func (i *CDCInstance) IsOwner(ctx context.Context, topo Topology, apiTimeoutSeconds int, cfg *tls.Config) (bool, error) {
	tidbTopo, ok := topo.(*Specification)
	if !ok {
		panic("topo should be type of tidb topology")
	}
	client := api.NewCDCOpenAPIClient(tidbTopo.GetCDCList(), 5*time.Second, cfg)

	owner, err := client.GetOwner()
	if err != nil {
		return false, perrs.Annotatef(err, "failed to get CDC owner %s", i.GetHost())
	}

	return owner.AdvertiseAddr == i.Host, nil
}
