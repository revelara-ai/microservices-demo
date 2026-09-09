// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

/*
As part of an optional Google Cloud demo, you can run an additional "packaging" microservice (HTTP server).
This file contains code related to the frontend and the "packaging" microservice.
*/

var (
	packagingServiceUrl string

	packagingHTTPClient = &http.Client{Timeout: packagingRequestTimeout}
)

type PackagingInfo struct {
	Weight float32 `json:"weight"`
	Width  float32 `json:"width"`
	Height float32 `json:"height"`
	Depth  float32 `json:"depth"`
}

// init() is a special function in Golang that will run when this package is imported.
func init() {
	packagingServiceUrl = os.Getenv("PACKAGING_SERVICE_URL")
}

func isPackagingServiceConfigured() bool {
	return packagingServiceUrl != ""
}

// httpGetPackagingInfo fetches optional packaging metadata. It runs inline in
// productHandler on every product page, so it carries the request context and
// an explicit budget: the sibling getAd call is bounded to 100ms and this
// optional lookup must not be able to outlive the page it decorates (RC-019).
func httpGetPackagingInfo(ctx context.Context, productId string) (*PackagingInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, packagingRequestTimeout)
	defer cancel()

	// Make the GET request
	url := packagingServiceUrl + "/" + productId
	fmt.Println("Requesting packaging info from URL: ", url)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := packagingHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Check the response status code
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Unexpected status code: %d", resp.StatusCode)
	}

	// Read the JSON response body, capped so an upstream cannot exhaust memory
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxPackagingResponseBytes))
	if err != nil {
		return nil, err
	}

	// Decode the JSON response into a PackagingInfo struct
	var packagingInfo PackagingInfo
	err = json.Unmarshal(responseBody, &packagingInfo)
	if err != nil {
		return nil, err
	}

	return &packagingInfo, nil
}
