/*
   Copyright 2020 The Compose Specification Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package loader

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/compose-spec/compose-go/errdefs"
	"github.com/compose-spec/compose-go/types"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// Normalize compose project by moving deprecated attributes to their canonical position and injecting implicit defaults
func Normalize(project *types.Project, resolvePaths bool) error {
	absWorkingDir, err := filepath.Abs(project.WorkingDir)
	if err != nil {
		return err
	}
	project.WorkingDir = absWorkingDir

	absComposeFiles, err := absComposeFiles(project.ComposeFiles)
	if err != nil {
		return err
	}
	project.ComposeFiles = absComposeFiles

	if project.Networks == nil {
		project.Networks = make(map[string]types.NetworkConfig)
	}

	// If not declared explicitly, Compose model involves an implicit "default" network
	if _, ok := project.Networks["default"]; !ok {
		project.Networks["default"] = types.NetworkConfig{}
	}

	err = relocateExternalName(project)
	if err != nil {
		return err
	}

	for i, s := range project.Services {
		if len(s.Networks) == 0 && s.NetworkMode == "" {
			// Service without explicit network attachment are implicitly exposed on default network
			s.Networks = map[string]*types.ServiceNetworkConfig{"default": nil}
		}

		if s.PullPolicy == types.PullPolicyIfNotPresent {
			s.PullPolicy = types.PullPolicyMissing
		}

		fn := func(s string) (string, bool) {
			v, ok := project.Environment[s]
			return v, ok
		}

		if s.Build != nil {
			if s.Build.Dockerfile == "" && s.Build.DockerfileInline == "" {
				s.Build.Dockerfile = "Dockerfile"
			}
			if resolvePaths {
				// Build context might be a remote http/git context. Unfortunately supported "remote"
				// syntax is highly ambiguous in moby/moby and not defined by compose-spec,
				// so let's assume runtime will check
				localContext := absPath(project.WorkingDir, s.Build.Context)
				if _, err := os.Stat(localContext); err == nil {
					s.Build.Context = localContext
				}
				for name, path := range s.Build.AdditionalContexts {
					if strings.Contains(path, "://") { // `docker-image://` or any builder specific context type
						continue
					}
					path = absPath(project.WorkingDir, path)
					if _, err := os.Stat(path); err == nil {
						s.Build.AdditionalContexts[name] = path
					}
				}
			}
			s.Build.Args = s.Build.Args.Resolve(fn)
		}
		for j, f := range s.EnvFile {
			s.EnvFile[j] = absPath(project.WorkingDir, f)
		}
		s.Environment = s.Environment.Resolve(fn)

		if s.Extends != nil && s.Extends.File != "" {
			s.Extends.File = absPath(project.WorkingDir, s.Extends.File)
		}

		for _, link := range s.Links {
			parts := strings.Split(link, ":")
			if len(parts) == 2 {
				link = parts[0]
			}
			s.DependsOn = setIfMissing(s.DependsOn, link, types.ServiceDependency{
				Condition: types.ServiceConditionStarted,
				Restart:   true,
			})
		}

		for _, namespace := range []string{s.NetworkMode, s.Ipc, s.Pid, s.Uts, s.Cgroup} {
			if strings.HasPrefix(namespace, types.ServicePrefix) {
				name := namespace[len(types.ServicePrefix):]
				s.DependsOn = setIfMissing(s.DependsOn, name, types.ServiceDependency{
					Condition: types.ServiceConditionStarted,
					Restart:   true,
				})
			}
		}

		for _, vol := range s.VolumesFrom {
			if !strings.HasPrefix(vol, types.ContainerPrefix) {
				spec := strings.Split(vol, ":")
				s.DependsOn = setIfMissing(s.DependsOn, spec[0], types.ServiceDependency{
					Condition: types.ServiceConditionStarted,
					Restart:   false,
				})
			}
		}

		err := relocateLogDriver(&s)
		if err != nil {
			return err
		}

		err = relocateLogOpt(&s)
		if err != nil {
			return err
		}

		err = relocateDockerfile(&s)
		if err != nil {
			return err
		}

		err = relocateScale(&s)
		if err != nil {
			return err
		}

		inferImplicitDependencies(&s)

		project.Services[i] = s
	}

	for name, config := range project.Volumes {
		if config.Driver == "local" && config.DriverOpts["o"] == "bind" {
			// This is actually a bind mount
			config.DriverOpts["device"] = absPath(project.WorkingDir, config.DriverOpts["device"])
			project.Volumes[name] = config
		}
	}

	setNameFromKey(project)

	return nil
}

// IsServiceDependency check the relation set by ref refers to a service
func IsServiceDependency(ref string) (string, bool) {
	if strings.HasPrefix(
		ref,
		types.ServicePrefix,
	) {
		return ref[len(types.ServicePrefix):], true
	}
	return "", false
}

func inferImplicitDependencies(service *types.ServiceConfig) {
	var dependencies []string

	maybeReferences := []string{
		service.NetworkMode,
		service.Ipc,
		service.Pid,
		service.Uts,
		service.Cgroup,
	}
	for _, ref := range maybeReferences {
		if dep, ok := IsServiceDependency(ref); ok {
			dependencies = append(dependencies, dep)
		}
	}

	for _, vol := range service.VolumesFrom {
		spec := strings.Split(vol, ":")
		if len(spec) == 0 {
			continue
		}
		if spec[0] == "container" {
			continue
		}
		dependencies = append(dependencies, spec[0])
	}

	for _, link := range service.Links {
		dependencies = append(dependencies, strings.Split(link, ":")[0])
	}

	if len(dependencies) > 0 && service.DependsOn == nil {
		service.DependsOn = make(types.DependsOnConfig)
	}

	for _, d := range dependencies {
		if _, ok := service.DependsOn[d]; !ok {
			service.DependsOn[d] = types.ServiceDependency{
				Condition: types.ServiceConditionStarted,
			}
		}
	}
}

// setIfMissing adds a ServiceDependency for service if not already defined
func setIfMissing(d types.DependsOnConfig, service string, dep types.ServiceDependency) types.DependsOnConfig {
	if d == nil {
		d = types.DependsOnConfig{}
	}
	if _, ok := d[service]; !ok {
		d[service] = dep
	}
	return d
}

func relocateScale(s *types.ServiceConfig) error {
	scale := uint64(s.Scale)
	if scale > 1 {
		logrus.Warn("`scale` is deprecated. Use the `deploy.replicas` element")
		if s.Deploy == nil {
			s.Deploy = &types.DeployConfig{}
		}
		if s.Deploy.Replicas != nil && *s.Deploy.Replicas != scale {
			return errors.Wrap(errdefs.ErrInvalid, "can't use both 'scale' (deprecated) and 'deploy.replicas'")
		}
		s.Deploy.Replicas = &scale
	}
	return nil
}

func absComposeFiles(composeFiles []string) ([]string, error) {
	absComposeFiles := make([]string, len(composeFiles))
	for i, composeFile := range composeFiles {
		absComposefile, err := filepath.Abs(composeFile)
		if err != nil {
			return nil, err
		}
		absComposeFiles[i] = absComposefile
	}
	return absComposeFiles, nil
}

// Resources with no explicit name are actually named by their key in map
func setNameFromKey(project *types.Project) {
	for i, n := range project.Networks {
		if n.Name == "" {
			n.Name = fmt.Sprintf("%s_%s", project.Name, i)
			project.Networks[i] = n
		}
	}

	for i, v := range project.Volumes {
		if v.Name == "" {
			v.Name = fmt.Sprintf("%s_%s", project.Name, i)
			project.Volumes[i] = v
		}
	}

	for i, c := range project.Configs {
		if c.Name == "" {
			c.Name = fmt.Sprintf("%s_%s", project.Name, i)
			project.Configs[i] = c
		}
	}

	for i, s := range project.Secrets {
		if s.Name == "" {
			s.Name = fmt.Sprintf("%s_%s", project.Name, i)
			project.Secrets[i] = s
		}
	}
}

func relocateExternalName(project *types.Project) error {
	for i, n := range project.Networks {
		if n.External.Name != "" {
			if n.Name != "" {
				return errors.Wrap(errdefs.ErrInvalid, "can't use both 'networks.external.name' (deprecated) and 'networks.name'")
			}
			n.Name = n.External.Name
		}
		project.Networks[i] = n
	}

	for i, v := range project.Volumes {
		if v.External.Name != "" {
			if v.Name != "" {
				return errors.Wrap(errdefs.ErrInvalid, "can't use both 'volumes.external.name' (deprecated) and 'volumes.name'")
			}
			v.Name = v.External.Name
		}
		project.Volumes[i] = v
	}

	for i, s := range project.Secrets {
		if s.External.Name != "" {
			if s.Name != "" {
				return errors.Wrap(errdefs.ErrInvalid, "can't use both 'secrets.external.name' (deprecated) and 'secrets.name'")
			}
			s.Name = s.External.Name
		}
		project.Secrets[i] = s
	}

	for i, c := range project.Configs {
		if c.External.Name != "" {
			if c.Name != "" {
				return errors.Wrap(errdefs.ErrInvalid, "can't use both 'configs.external.name' (deprecated) and 'configs.name'")
			}
			c.Name = c.External.Name
		}
		project.Configs[i] = c
	}
	return nil
}

func relocateLogOpt(s *types.ServiceConfig) error {
	if len(s.LogOpt) != 0 {
		logrus.Warn("`log_opts` is deprecated. Use the `logging` element")
		if s.Logging == nil {
			s.Logging = &types.LoggingConfig{}
		}
		for k, v := range s.LogOpt {
			if _, ok := s.Logging.Options[k]; !ok {
				s.Logging.Options[k] = v
			} else {
				return errors.Wrap(errdefs.ErrInvalid, "can't use both 'log_opt' (deprecated) and 'logging.options'")
			}
		}
	}
	return nil
}

func relocateLogDriver(s *types.ServiceConfig) error {
	if s.LogDriver != "" {
		logrus.Warn("`log_driver` is deprecated. Use the `logging` element")
		if s.Logging == nil {
			s.Logging = &types.LoggingConfig{}
		}
		if s.Logging.Driver == "" {
			s.Logging.Driver = s.LogDriver
		} else {
			return errors.Wrap(errdefs.ErrInvalid, "can't use both 'log_driver' (deprecated) and 'logging.driver'")
		}
	}
	return nil
}

func relocateDockerfile(s *types.ServiceConfig) error {
	if s.Dockerfile != "" {
		logrus.Warn("`dockerfile` is deprecated. Use the `build` element")
		if s.Build == nil {
			s.Build = &types.BuildConfig{}
		}
		if s.Dockerfile == "" {
			s.Build.Dockerfile = s.Dockerfile
		} else {
			return errors.Wrap(errdefs.ErrInvalid, "can't use both 'dockerfile' (deprecated) and 'build.dockerfile'")
		}
	}
	return nil
}