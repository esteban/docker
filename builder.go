package docker

import (
	"fmt"
	"github.com/dotcloud/docker/utils"
	"os"
	"path"
	"strings"
	"time"
)

var defaultDns = []string{"8.8.8.8", "8.8.4.4"}

type Builder struct {
	runtime      *Runtime
	repositories *TagStore
	graph        *Graph

	config *Config
	image  *Image
}

func NewBuilder(runtime *Runtime) *Builder {
	return &Builder{
		runtime:      runtime,
		graph:        runtime.graph,
		repositories: runtime.repositories,
	}
}

func (builder *Builder) Create(config *Config) (*Container, []string, error) {
	// Lookup image
	img, err := builder.repositories.LookupImage(config.Image)
	if err != nil {
		return nil, nil, err
	}
	warnings := []string{}

	if img.Config != nil {
		if img.Config.PortSpecs != nil && warnings != nil {
			for _, p := range img.Config.PortSpecs {
				if strings.Contains(p, ":") {
					warnings = append(warnings, "This image expects private ports to be mapped to public ports on your host. "+
						"This has been deprecated and the public mappings will not be honored."+
						"Use -p to publish the ports.")
					break
				}
			}
		}
		MergeConfig(config, img.Config)
	}

	if len(config.Entrypoint) != 0 && config.Cmd == nil {
		config.Cmd = []string{}
	} else if config.Cmd == nil || len(config.Cmd) == 0 {
		return nil, nil, fmt.Errorf("No command specified")
	}

	// Generate id
	id := GenerateID()
	// Generate default hostname
	// FIXME: the lxc template no longer needs to set a default hostname
	if config.Hostname == "" {
		config.Hostname = id[:12]
	}

	var args []string
	var entrypoint string

	if len(config.Entrypoint) != 0 {
		entrypoint = config.Entrypoint[0]
		args = append(config.Entrypoint[1:], config.Cmd...)
	} else {
		entrypoint = config.Cmd[0]
		args = config.Cmd[1:]
	}

	container := &Container{
		// FIXME: we should generate the ID here instead of receiving it as an argument
		ID:              id,
		Created:         time.Now(),
		Path:            entrypoint,
		Args:            args, //FIXME: de-duplicate from config
		Config:          config,
		Image:           img.ID, // Always use the resolved image id
		NetworkSettings: &NetworkSettings{},
		// FIXME: do we need to store this in the container?
		SysInitPath: sysInitPath,
	}
	container.root = builder.runtime.containerRoot(container.ID)
	// Step 1: create the container directory.
	// This doubles as a barrier to avoid race conditions.
	if err := os.Mkdir(container.root, 0700); err != nil {
		return nil, nil, err
	}

	resolvConf, err := utils.GetResolvConf()
	if err != nil {
		return nil, nil, err
	}

	if len(config.Dns) == 0 && len(builder.runtime.config.Dns) == 0 && utils.CheckLocalDns(resolvConf) {
		//"WARNING: Docker detected local DNS server on resolv.conf. Using default external servers: %v", defaultDns
		builder.runtime.config.Dns = defaultDns
	}

	// If custom dns exists, then create a resolv.conf for the container
	if len(config.Dns) > 0 || len(builder.runtime.config.Dns) > 0 {
		var dns []string
		if len(config.Dns) > 0 {
			dns = config.Dns
		} else {
			dns = builder.runtime.config.Dns
		}
		container.ResolvConfPath = path.Join(container.root, "resolv.conf")
		f, err := os.Create(container.ResolvConfPath)
		if err != nil {
			return nil, nil, err
		}
		defer f.Close()
		for _, dns := range dns {
			if _, err := f.Write([]byte("nameserver " + dns + "\n")); err != nil {
				return nil, nil, err
			}
		}
	} else {
		container.ResolvConfPath = "/etc/resolv.conf"
	}

	// Step 2: save the container json
	if err := container.ToDisk(); err != nil {
		return nil, nil, err
	}
	// Step 3: register the container
	if err := builder.runtime.Register(container); err != nil {
		return nil, nil, err
	}
	return container, warnings, nil
}

// Commit creates a new filesystem image from the current state of a container.
// The image can optionally be tagged into a repository
func (builder *Builder) Commit(container *Container, repository, tag, comment, author string, config *Config) (*Image, error) {
	// FIXME: freeze the container before copying it to avoid data corruption?
	// FIXME: this shouldn't be in commands.
	if err := container.EnsureMounted(); err != nil {
		return nil, err
	}

	rwTar, err := container.ExportRw()
	if err != nil {
		return nil, err
	}
	// Create a new image from the container's base layers + a new layer from container changes
	img, err := builder.graph.Create(rwTar, container, comment, author, config)
	if err != nil {
		return nil, err
	}
	// Register the image if needed
	if repository != "" {
		if err := builder.repositories.Set(repository, tag, img.ID, true); err != nil {
			return img, err
		}
	}
	return img, nil
}
