// Copyright 2025 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import "github.com/GoogleCloudPlatform/khi/pkg/parameters"

// GetConfigResponse is the response type of /api/v3/config
type GetConfigResponse struct {
	// ViewerMode is a flag indicating if the server is the viewer mode and not accepting creating a new inspection request.
	ViewerMode bool `json:"viewerMode"`
	// InitialInspectionID is set when the server was started with --viewer-inspection-id. The frontend should fetch and display this inspection on load.
	InitialInspectionID string `json:"initialInspectionId,omitempty"`
}

// NewGetConfigResponseFromParameters returns *GetConfigResponse created from given program parameters.
func NewGetConfigResponseFromParameters() *GetConfigResponse {
	isViewerMode := false
	if parameters.Server.ViewerMode != nil {
		isViewerMode = *parameters.Server.ViewerMode
	}
	initialInspectionID := ""
	if parameters.Server.ViewerInspectionID != nil && *parameters.Server.ViewerInspectionID != "" {
		initialInspectionID = *parameters.Server.ViewerInspectionID
	}
	return &GetConfigResponse{
		ViewerMode:         isViewerMode,
		InitialInspectionID: initialInspectionID,
	}
}
