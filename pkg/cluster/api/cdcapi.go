// Copyright 2022 PingCAP, Inc.
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

package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/pingcap/errors"
	logprinter "github.com/pingcap/tiup/pkg/logger/printer"
	"github.com/pingcap/tiup/pkg/utils"
)

// CDCOpenAPIClient is client for access TiCDC Open API
type CDCOpenAPIClient struct {
	urls   []string
	client *utils.HTTPClient
	ctx    context.Context
}

// NewCDCOpenAPIClient return a `CDCOpenAPIClient`
func NewCDCOpenAPIClient(ctx context.Context, addresses []string, timeout time.Duration, tlsConfig *tls.Config) *CDCOpenAPIClient {
	httpPrefix := "http"
	if tlsConfig != nil {
		httpPrefix = "https"
	}
	urls := make([]string, 0, len(addresses))
	for _, addr := range addresses {
		urls = append(urls, fmt.Sprintf("%s://%s", httpPrefix, addr))
	}

	return &CDCOpenAPIClient{
		urls:   urls,
		client: utils.NewHTTPClient(timeout, tlsConfig),
		ctx:    ctx,
	}
}

func (c *CDCOpenAPIClient) getEndpoints(api string) (endpoints []string) {
	for _, url := range c.urls {
		endpoints = append(endpoints, fmt.Sprintf("%s/%s", url, api))
	}
	return endpoints
}

func drainCapture(client *CDCOpenAPIClient, target string) (int, error) {
	api := "api/v1/captures/drain"
	endpoints := client.getEndpoints(api)

	request := DrainCaptureRequest{
		CaptureID: target,
	}
	body, err := json.Marshal(request)
	if err != nil {
		return 0, err
	}

	var resp DrainCaptureResp
	_, err = tryURLs(endpoints, func(endpoint string) ([]byte, error) {
		data, statusCode, err := client.client.Put(client.ctx, endpoint, bytes.NewReader(body))
		if err != nil {
			if statusCode == http.StatusNotFound {
				// old version cdc does not support `DrainCapture`, return nil to trigger hard restart.
				client.l().Debugf("cdc drain capture does not support, ignore it, target: %s, err: %s", target, err)
				return data, nil
			}
			// match https://github.com/pingcap/tiflow/blob/e3d0d9d23b77c7884b70016ddbd8030ffeb95dfd/pkg/errors/cdc_errors.go#L55-L57
			if bytes.Contains(data, []byte("scheduler request failed")) {
				client.l().Debugf("cdc drain capture failed, data: %s", data)
				return data, nil
			}
			// match https://github.com/pingcap/tiflow/blob/e3d0d9d23b77c7884b70016ddbd8030ffeb95dfd/pkg/errors/cdc_errors.go#L51-L54
			if bytes.Contains(data, []byte("capture not exists")) {
				client.l().Debugf("cdc drain capture failed, data: %s", data)
				return data, nil
			}
			client.l().Debugf("cdc drain capture failed, data: %s, statusCode: %d, err: %s", data, statusCode, err)
			return data, err
		}
		return data, json.Unmarshal(data, &resp)
	})
	return resp.CurrentTableCount, err
}

// DrainCapture request cdc owner move all tables on the target capture to other captures.
func (c *CDCOpenAPIClient) DrainCapture(target string, apiTimeoutSeconds int) error {
	start := time.Now()
	err := utils.Retry(func() error {
		count, err := drainCapture(c, target)
		if err != nil {
			return err
		}
		if count == 0 {
			return nil
		}
		c.l().Debugf("DrainCapture not finished, target: %s, count: %d", target, count)
		return errors.New("still waiting for the drain capture to finish")
	}, utils.RetryOption{
		Delay:   2 * time.Second,
		Timeout: time.Duration(apiTimeoutSeconds) * time.Second,
	})

	if err != nil {
		c.l().Warnf("cdc drain capture not success, give up, target: %s, err: %s", target, err)
	}

	c.l().Infof("cdc drain capture finished, target: %s, elapsed: %+v", target, time.Since(start))
	return nil
}

// ResignOwner resign the cdc owner, and wait for a new owner be found
func (c *CDCOpenAPIClient) ResignOwner() error {
	api := "api/v1/owner/resign"
	endpoints := c.getEndpoints(api)
	_, err := tryURLs(endpoints, func(endpoint string) ([]byte, error) {
		body, statusCode, err := c.client.PostWithStatusCode(c.ctx, endpoint, nil)
		if err != nil {
			if statusCode == http.StatusNotFound {
				c.l().Debugf("resign owner does not found, data: %s, statusCode: %d, err: %s", body, statusCode, err)
				return body, nil
			}
			c.l().Warnf("resign owner failed, data: %s, statusCode: %d, err: %s", body, statusCode, err)
			return body, err
		}
		return body, nil
	})

	if err != nil {
		return err
	}

	owner, err := c.GetOwner()
	if err != nil {
		c.l().Warnf("cdc get owner failed, err: %s", err)
	}

	if owner.IsOwner {
		c.l().Infof("cdc resign owner successfully, and new owner found, owner: %s", owner)
	}
	return err
}

// GetOwner return the cdc owner capture information
func (c *CDCOpenAPIClient) GetOwner() (*Capture, error) {
	captures, err := c.GetAllCaptures()
	if err != nil {
		return nil, err
	}

	for _, capture := range captures {
		if capture.IsOwner {
			return capture, nil
		}
	}
	return nil, errors.New("owner not found")
}

// GetCaptureByAddr return the capture information by the address
func (c *CDCOpenAPIClient) GetCaptureByAddr(addr string) (*Capture, error) {
	captures, err := c.GetAllCaptures()
	if err != nil {
		return nil, err
	}

	for _, capture := range captures {
		if capture.AdvertiseAddr == addr {
			return capture, nil
		}
	}

	return nil, fmt.Errorf("capture not found, addr: %s", addr)
}

// GetAllCaptures return all captures instantaneously
func (c *CDCOpenAPIClient) GetAllCaptures() (result []*Capture, err error) {
	// todo: remove this retry
	err = utils.Retry(func() error {
		result, err = getAllCaptures(c)
		if err != nil {
			return err
		}
		return nil
	}, utils.RetryOption{
		Timeout: 20 * time.Second,
	})
	if err != nil {
		// todo: set to debug level
		c.l().Warnf("cdc get all captures failed, err: %s", err)
	}

	return result, err
}

// GetStatus return the status of the TiCDC server.
func (c *CDCOpenAPIClient) GetStatus() (result Liveness, err error) {
	err = utils.Retry(func() error {
		result, err = getCDCServerStatus(c)
		if err != nil {
			return err
		}
		return nil
	}, utils.RetryOption{
		Timeout: 20 * time.Second,
	})

	if err != nil {
		c.l().Warnf("cdc get capture status failed, err: %s", err)
	}

	return result, err
}

func getCDCServerStatus(client *CDCOpenAPIClient) (Liveness, error) {
	api := "api/v1/status"
	// client should only have address to the target cdc server, not all cdc servers.
	endpoints := client.getEndpoints(api)

	var response ServerStatus
	data, err := client.client.Get(client.ctx, endpoints[0])
	if err != nil {
		return response.Liveness, err
	}

	err = json.Unmarshal(data, &response)
	if err != nil {
		return response.Liveness, err
	}

	return response.Liveness, nil
}

func getAllCaptures(client *CDCOpenAPIClient) ([]*Capture, error) {
	api := "api/v1/captures"
	endpoints := client.getEndpoints(api)

	var response []*Capture

	_, err := tryURLs(endpoints, func(endpoint string) ([]byte, error) {
		body, statusCode, err := client.client.GetWithStatusCode(client.ctx, endpoint)
		// todo: remove this log
		if err != nil {
			if statusCode == http.StatusNotFound {
				// old version cdc does not support open api, also the stopped cdc instance
				// return nil to trigger hard restart
				client.l().Warnf("get all captures not support, ignore: %s, statusCode: %d, err: %s", body, statusCode, err)
				return body, nil
			}
			return body, err
		}

		return body, json.Unmarshal(body, &response)
	})

	if err != nil {
		return nil, err
	}
	return response, nil
}

func (c *CDCOpenAPIClient) l() *logprinter.Logger {
	return c.ctx.Value(logprinter.ContextKeyLogger).(*logprinter.Logger)
}

// Liveness is the liveness status of a capture.
type Liveness int32

const (
	// LivenessCaptureAlive means the capture is alive, and ready to serve.
	LivenessCaptureAlive Liveness = 0
	// LivenessCaptureStopping means the capture is in the process of graceful shutdown.
	LivenessCaptureStopping Liveness = 1
)

// ServerStatus holds some common information of a TiCDC server
type ServerStatus struct {
	Version  string   `json:"version"`
	GitHash  string   `json:"git_hash"`
	ID       string   `json:"id"`
	Pid      int      `json:"pid"`
	IsOwner  bool     `json:"is_owner"`
	Liveness Liveness `json:"liveness"`
}

// Capture holds common information of a capture in cdc
type Capture struct {
	ID            string `json:"id"`
	IsOwner       bool   `json:"is_owner"`
	AdvertiseAddr string `json:"address"`
}

// DrainCaptureRequest is request for manual `DrainCapture`
type DrainCaptureRequest struct {
	CaptureID string `json:"capture_id"`
}

// DrainCaptureResp is response for manual `DrainCapture`
type DrainCaptureResp struct {
	CurrentTableCount int `json:"current_table_count"`
}
