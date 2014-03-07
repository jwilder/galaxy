package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/coreos/go-etcd/etcd"
	"github.com/jwilder/go-dockerclient"
	"github.com/litl/galaxy/commander/auth"
	"os"
	"os/user"
	"path"
	"strings"
	"time"
)

var (
	client         *docker.Client
	ectdClient     *etcd.Client
	stopCutoff     = flag.Int64("cutoff", 5*60, "Seconds to wait before stopping old containers")
	image          = flag.String("t", "", "Image to start")
	etcdHosts      = flag.String("etcd", "http://127.0.0.1:4001", "Comma-separated list of etcd hosts")
	env            = flag.String("env", "dev", "Environment namespace")
	pool           = flag.String("pool", "web", "Pool namespace")
	authConfig     *auth.ConfigFile
	serviceConfigs []*ServiceConfig
	hostname       string
)

type ServiceConfig struct {
	Name    string
	Version string
	Env     map[string]string
}

func splitDockerImage(img string) (string, string, string) {
	if !strings.Contains(img, "/") {
		return "", img, ""
	}
	parts := strings.Split(img, "/")

	if !strings.Contains(parts[1], ":") {
		return parts[0], parts[0] + "/" + parts[1], ""
	}

	imageParts := strings.Split(parts[1], ":")
	// registry, repository, tag
	return parts[0], parts[0] + "/" + imageParts[0], imageParts[1]
}

func isRunning(img string) (string, error) {

	image, err := client.InspectImage(img)
	if err != nil {
		return "", err
	}

	containers, err := client.ListContainers(docker.ListContainersOptions{
		All: false,
	})
	if err != nil {
		return "", err
	}

	for _, container := range containers {
		dockerContainer, err := client.InspectContainer(container.ID)
		if err != nil {
			return "", err
		}

		if image.ID == dockerContainer.Image {
			return container.ID, nil
		}
	}
	return "", nil
}

func pullImage(registry, repository string) error {
	// No, pull it down locally
	pullOpts := docker.PullImageOptions{
		Repository:   repository,
		Registry:     registry,
		OutputStream: os.Stdout}

	// use .dockercfg if available
	auth := docker.AuthConfiguration{}
	if registry != "" {
		pullOpts.Registry = registry
		authCreds := authConfig.ResolveAuthConfig(registry)

		auth.Username = authCreds.Username
		auth.Password = authCreds.Password
		auth.Email = authCreds.Email

	}

	return client.PullImage(pullOpts, auth)

}

func startIfNotRunning(serviceConfig *ServiceConfig) (*docker.Container, error) {
	img := serviceConfig.Version
	containerId, err := isRunning(img)
	if err != nil && err != docker.ErrNoSuchImage {
		return nil, err
	}

	// already running, grab the container details
	if containerId != "" {
		return client.InspectContainer(containerId)
	}

	registry, repository, _ := splitDockerImage(img)

	// see if we have the image locally
	_, err = client.InspectImage(img)

	if err == docker.ErrNoSuchImage {
		err := pullImage(registry, repository)
		if err != nil {
			return nil, err
		}
	}

	// setup env vars from etcd
	var envVars []string
	for key, value := range serviceConfig.Env {
		envVars = append(envVars, strings.ToUpper(key)+"="+value)
	}
	container, err := client.CreateContainer(docker.CreateContainerOptions{
		Config: &docker.Config{
			Image: img,
			Env:   envVars,
		},
	})
	if err != nil {
		return nil, err
	}

	err = client.StartContainer(container.ID,
		&docker.HostConfig{})

	if err != nil {
		return container, err
	}

	return client.InspectContainer(containerId)
}

func getImageByName(img string) (*docker.APIImages, error) {
	imgs, err := client.ListImages(true)
	if err != nil {
		panic(err)
	}

	for _, image := range imgs {
		if stringInSlice(img, image.RepoTags) {
			return &image, nil
		}
	}
	return nil, nil

}

func stringInSlice(a string, list []string) bool {
	for _, b := range list {
		if b == a {
			return true
		}
	}
	return false
}

func stopAllButLatest(img string, latest *docker.Container) error {
	imageParts := strings.Split(img, ":")
	repository := imageParts[0]

	containers, err := client.ListContainers(docker.ListContainersOptions{
		All:    false,
		Before: latest.ID,
	})
	if err != nil {
		return err
	}
	for _, container := range containers {

		if strings.HasPrefix(container.Image, repository) && container.ID != latest.ID &&
			container.Created < (time.Now().Unix()-*stopCutoff) {
			err := client.StopContainer(container.ID, 10)
			if err != nil {
				fmt.Printf("ERROR: Unable to stop container: %s\n", container.ID)
			}
			client.RemoveContainer(docker.RemoveContainerOptions{
				ID:            container.ID,
				RemoveVolumes: true,
			})
		}
	}
	return nil

}

func buildServiceConfigs() []*ServiceConfig {
	var serviceConfigs []*ServiceConfig

	resp, err := ectdClient.Get("/"+*env+"/"+*pool, false, true)
	if err != nil {
		fmt.Printf("ERROR: Could not retrieve service config: %s\n", err)
		return serviceConfigs
	}
	for _, node := range resp.Node.Nodes {
		service := path.Base(node.Key)

		if service == "hosts" {
			continue
		}

		serviceConfig := &ServiceConfig{
			Name: service,
			Env:  make(map[string]string),
		}
		fmt.Printf("Detected service %s\n", service)

		for _, configKey := range node.Nodes {
			if strings.HasSuffix(configKey.Key, "/version") {
				*image = configKey.Value
				serviceConfig.Version = configKey.Value
			} else if strings.HasSuffix(configKey.Key, "/environment") {
				err := json.Unmarshal([]byte(configKey.Value), &serviceConfig.Env)
				if err != nil {
					fmt.Printf("ERROR: Could not unmarshall config: %s\n", err)
					return serviceConfigs
				}
			} else {
				fmt.Printf("WARN: Unknown entry %s. Ignoring\n", configKey.Key)
			}
		}
		serviceConfigs = append(serviceConfigs, serviceConfig)
	}
	return serviceConfigs
}

func initOrDie() {
	var err error
	endpoint := "unix:///var/run/docker.sock"
	client, err = docker.NewClient(endpoint)

	if err != nil {
		panic(err)
	}

	currentUser, err := user.Current()
	if err != nil {
		panic(err)
	}

	hostname, err = os.Hostname()
	if err != nil {
		panic(err)
	}

	// use ~/.dockercfg
	authConfig, err = auth.LoadConfig(currentUser.HomeDir)
	if err != nil {
		panic(err)
	}
}

func main() {
	flag.Parse()

	if *env == "" {
		fmt.Println("Need an env")
		flag.PrintDefaults()
		os.Exit(1)
	}

	if *pool == "" {
		fmt.Println("Need a pool")
		flag.PrintDefaults()
		os.Exit(1)
	}

	initOrDie()

	if *image == "" && *etcdHosts != "" {
		machines := strings.Split(*etcdHosts, ",")
		ectdClient = etcd.NewClient(machines)

		serviceConfigs = buildServiceConfigs()
	}

	if len(serviceConfigs) == 0 {
		fmt.Printf("No services configured for /%s/%s\n", *env, *pool)
		os.Exit(0)
	}

	for _, serviceConfig := range serviceConfigs {
		if serviceConfig.Version == "" {
			fmt.Printf("Skipping %s. No version configured.\n", serviceConfig.Name)
			continue
		}

		container, err := startIfNotRunning(serviceConfig)
		if err != nil {
			fmt.Printf("ERROR: Could not determine if %s is running: %s\n",
				serviceConfig.Version, err)
			os.Exit(1)
		}

		fmt.Printf("%s running as %s\n", serviceConfig.Version, container.ID)

		stopAllButLatest(serviceConfig.Version, container)

	}

}