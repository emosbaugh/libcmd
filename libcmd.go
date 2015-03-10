package libcmd

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httputil"
	"reflect"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/fsouza/go-dockerclient"
)

var (
	ErrCommandResponse = errors.New("error running command")

	globalDockerClient *docker.Client
	config             CmdConfig

	cmdConfigDefaultOpts = map[string]string{
		"CommandsDir":         "/root/commands",
		"DockerEndpoint":      "unix:///var/run/docker.sock",
		"ContainerRepository": "freighterio/cmd",
		"ContainerTag":        "latest",
	}
)

type Cmd struct {
	op           string
	dockerClient *docker.Client
}

type CmdConfig struct {
	CommandsDir         string
	DockerEndpoint      string
	ContainerRepository string
	ContainerTag        string
}

func InitCmdContainer(opts map[string]string) {
	config = CmdConfig{}
	for key, dflt := range cmdConfigDefaultOpts {
		field := reflect.ValueOf(&config).Elem().FieldByName(key)
		if value, ok := opts[key]; ok {
			field.SetString(value)
		} else {
			field.SetString(dflt)
		}
	}

	client, err := docker.NewClient(config.DockerEndpoint)
	if err != nil {
		log.Fatal(err)
	}
	globalDockerClient = client
	if err := pullImage(globalDockerClient, config.ContainerRepository, config.ContainerTag); err != nil {
		log.Fatal(err)
	}
}

func NewCmd(op string) *Cmd {
	cmd := Cmd{op, globalDockerClient}
	return &cmd
}

func (c *Cmd) Run(args ...string) (string, error) {
	cmd := []string{"bash", fmt.Sprintf("%s/%s.sh", config.CommandsDir, c.op)}
	cmd = append(cmd, args...)
	container, err := createContainer(c.dockerClient, config.ContainerRepository, config.ContainerTag, cmd)
	if err != nil {
		return "", err
	}
	defer removeContainer(c.dockerClient, container.ID)

	if err := startContainer(c.dockerClient, container.ID); err != nil {
		return "", err
	}

	nilTime := time.Time{}
	for {
		cntr, err := c.dockerClient.InspectContainer(container.ID)
		if err != nil {
			return "", err
		}
		if !cntr.State.FinishedAt.Equal(nilTime) {
			break
		}
		time.Sleep(time.Millisecond + 100)
	}

	logs, err := getLogs(c.dockerClient, container.ID)
	if err != nil {
		return "", err
	}
	return logs, nil
}

func pullImage(client *docker.Client, repository, tag string) error {
	reader, writer := io.Pipe()
	go func(reader io.Reader) {
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			line := scanner.Text()
			log.Debugf(" -> %s", line)
		}
	}(reader)
	opts := docker.PullImageOptions{
		Repository:   repository,
		Tag:          tag,
		OutputStream: writer,
	}
	log.Infof("pulling image %s:%s", repository, tag)
	if err := client.PullImage(opts, docker.AuthConfiguration{}); err != nil {
		return err
	}
	log.Infof(" -> pulling image %s:%s complete", repository, tag)
	return nil
}

func createContainer(client *docker.Client, repository, tag string, cmd []string) (*docker.Container, error) {
	log.Infof("creating container %s:%s", repository, tag)
	config := &docker.Config{
		Image: fmt.Sprintf("%s:%s", repository, tag),
		Cmd:   cmd,
	}
	opts := docker.CreateContainerOptions{
		Config: config,
	}
	container, err := client.CreateContainer(opts)
	if err != nil {
		log.Errorf(" -> error creating container %s:%s: %s", repository, tag, err)
		return nil, err
	}
	log.Infof(" -> container %s:%s with id %s created", repository, tag, container.ID)
	return container, nil

}

func startContainer(client *docker.Client, containerId string) error {
	log.Infof("starting container %s", containerId)
	hostConfig := &docker.HostConfig{}
	if err := client.StartContainer(containerId, hostConfig); err != nil {
		log.Errorf(" -> error starting container %s: %s", containerId, err)
		return err
	}
	log.Infof(" -> container %s started", containerId)
	return nil
}

func removeContainer(client *docker.Client, containerId string) error {
	log.Infof("remove container %s", containerId)
	opts := docker.RemoveContainerOptions{
		ID:            containerId,
		RemoveVolumes: false,
		Force:         true,
	}
	if err := client.RemoveContainer(opts); err != nil {
		log.Errorf(" -> error removing container %s: %s", containerId, err)
		return err
	}
	log.Infof(" -> container %s removed", containerId)
	return nil
}

func getLogs(client *docker.Client, containerId string) (string, error) {
	log.Infof("getting container %s logs", containerId)
	stdout, stderr, _, err := makeRequest("GET", fmt.Sprintf("/containers/%s/logs?follow=0&stderr=1&stdout=1", containerId))
	if err != nil {
		log.Errorf(" -> error making container %s logs request: %s", containerId, err)
		return "", err
	}
	if len(stderr) != 0 {
		log.Errorf(" -> error running container %s command: %s", containerId, stderr)
		return string(stderr), ErrCommandResponse
	}
	log.Infof(" -> container %s logs request complete", containerId)
	return string(stdout), nil
}

func makeRequest(method, path string) ([]byte, []byte, int, error) {
	req, err := http.NewRequest(method, path, nil)
	if err != nil {
		return nil, nil, -1, err
	}
	req.Header.Set("User-Agent", "go-dockerclient")
	var resp *http.Response
	address := "/var/run/docker.sock"
	dial, err := net.Dial("unix", address)
	if err != nil {
		return nil, nil, -1, err
	}
	defer dial.Close()
	clientconn := httputil.NewClientConn(dial, nil)
	resp, err = clientconn.Do(req)
	if err != nil {
		return nil, nil, -1, err
	}
	defer clientconn.Close()
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") {
			return nil, nil, -1, docker.ErrConnectionRefused
		}
		return nil, nil, -1, err
	}
	var stdoutBuffer, stderrBuffer bytes.Buffer
	if _, err := stdCopy(&stdoutBuffer, &stderrBuffer, resp.Body); err != nil {
		return nil, nil, -1, err
	}
	bErr, _ := ioutil.ReadAll(&stderrBuffer)
	bOut, err := ioutil.ReadAll(&stdoutBuffer)
	return bOut, bErr, resp.StatusCode, err
}
