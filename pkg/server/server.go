// Copyright 2024 Google LLC
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

package server

import (
	"embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/khi/pkg/common/filter"
	"github.com/GoogleCloudPlatform/khi/pkg/common/typedmap"
	coreinspection "github.com/GoogleCloudPlatform/khi/pkg/core/inspection"
	inspectionmetadata "github.com/GoogleCloudPlatform/khi/pkg/core/inspection/metadata"
	"github.com/GoogleCloudPlatform/khi/pkg/parameters"
	"github.com/GoogleCloudPlatform/khi/pkg/server/config"
	"github.com/GoogleCloudPlatform/khi/pkg/server/popup"
	"github.com/GoogleCloudPlatform/khi/pkg/server/upload"
	inspectioncore_contract "github.com/GoogleCloudPlatform/khi/pkg/task/inspection/inspectioncore/contract"

	"github.com/gin-contrib/static"
	"github.com/gin-gonic/gin"
)

const embeddedStaticFolderPath = "dist/browser"

//go:embed dist/browser
var embeddedStaticFolder embed.FS

type ServerConfig struct {
	ViewerMode            bool
	StaticFolderPath      string
	ResourceMonitor       ResourceMonitor
	ServerBasePath        string
	UploadFileStore       *upload.UploadFileStore
	DataDestinationFolder string
}

func redirectMiddleware(exactPath string, redirectTo string) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		if ctx.Request.URL.Path == exactPath {
			ctx.Redirect(302, redirectTo)
			return
		}
		ctx.Next()
	}
}

func CreateKHIServer(engine *gin.Engine, inspectionServer *coreinspection.InspectionTaskServer, serverConfig *ServerConfig) *gin.Engine {
	basePathWithoutTrailingSlash := strings.TrimSuffix(serverConfig.ServerBasePath, "/")
	engine.Use(redirectMiddleware(basePathWithoutTrailingSlash+"/", basePathWithoutTrailingSlash+"/session/0")) // Request for `/` shouldn't be handled by `static.Serve`, redirect `/session/0` to be handled by patternToString

	// By default, use the embedded web files. If the static folder path is set, use the local file system.
	appHtmlPath := path.Join(embeddedStaticFolderPath, "/index.html")
	webFS := embedFolder(embeddedStaticFolder, embeddedStaticFolderPath)
	webFSDebugMessage := "Using embedded static web files."
	fileReaderFunc := embeddedStaticFolder.ReadFile
	if serverConfig.StaticFolderPath != "" {
		appHtmlPath = path.Join(serverConfig.StaticFolderPath, "/index.html")
		webFS = static.LocalFile(serverConfig.StaticFolderPath, false)
		webFSDebugMessage = fmt.Sprintf("Using local file system for static web files from %s", serverConfig.StaticFolderPath)
		fileReaderFunc = os.ReadFile
	}
	slog.Debug(webFSDebugMessage)
	engine.Use(static.Serve(basePathWithoutTrailingSlash+"/", webFS))

	router := engine.Group(basePathWithoutTrailingSlash)

	// frontend uses Angular router. All frontend routing path should return the app html
	router.GET("/session/*wild", func(ctx *gin.Context) {
		ctx.Header("Content-Type", "text/html")
		file, err := fileReaderFunc(appHtmlPath)
		if err != nil {
			ctx.String(http.StatusInternalServerError, err.Error())
			return
		}
		originalIndexHTML := string(file)
		replacedIndexHtml, err := replaceDynamicPartOfIndex(originalIndexHTML)
		if err != nil {
			ctx.String(http.StatusInternalServerError, err.Error())
			return
		}
		ctx.Writer.Write([]byte(replacedIndexHtml))
	})
	// GET /api/v3/config
	// Returns configuration map used in frontend.
	router.GET("/api/v3/config", func(ctx *gin.Context) {
		ctx.JSON(http.StatusOK, config.NewGetConfigResponseFromParameters())
	})

	if !serverConfig.ViewerMode {
		// GET /api/v3/inspection/types
		// Returns the list of inspection types available on the inspection server.
		router.GET("/api/v3/inspection/types", func(ctx *gin.Context) {
			ctx.JSON(http.StatusOK, &GetInspectionTypesResponse{
				Types: inspectionServer.GetAllInspectionTypes(),
			})
		})

		// POST /api/v3/inspection/tasks
		router.POST("/api/v3/inspection/types/:typeID", func(ctx *gin.Context) {
			typeID := ctx.Param("typeID")
			inspectionId, err := inspectionServer.CreateInspection(typeID)
			if err != nil {
				// only the not found error is expected here
				ctx.String(http.StatusNotFound, err.Error())
				return
			}
			ctx.JSON(http.StatusAccepted, &PostInspectionResponse{InspectionID: inspectionId})
		})
		// PATCH /api/v3/inspection/<inspection-id>
		router.PATCH("/api/v3/inspection/:inspectionID", func(ctx *gin.Context) {
			inspectionID := ctx.Param("inspectionID")
			task := inspectionServer.GetInspection(inspectionID)
			if task == nil {
				ctx.String(http.StatusNotFound, fmt.Sprintf("inspection %s was not found", inspectionID))
				return
			}
			var reqBody PatchInspectionRequest
			if err := ctx.ShouldBindJSON(&reqBody); err != nil {
				ctx.String(http.StatusBadRequest, err.Error())
				return
			}
			md, err := task.GetCurrentMetadata()
			if err != nil {
				ctx.String(http.StatusBadRequest, err.Error())
				return
			}
			var header *inspectionmetadata.HeaderMetadata
			header, found := typedmap.Get(md, inspectionmetadata.HeaderMetadataKey)
			if !found {
				ctx.String(http.StatusBadRequest, "header not found")
				return
			}
			header.InspectionName = reqBody.Name
			header.SuggestedFileName = fmt.Sprintf("%s.khi", reqBody.Name)
			ctx.String(http.StatusAccepted, "ok")
		})
		// PUT /api/v3/inspection/<inspection-id>/features
		router.PUT("/api/v3/inspection/:inspectionID/features", func(ctx *gin.Context) {
			inspectionID := ctx.Param("inspectionID")
			task := inspectionServer.GetInspection(inspectionID)
			if task == nil {
				ctx.String(http.StatusNotFound, fmt.Sprintf("inspecton %s was not found", inspectionID))
				return
			}
			var reqBody PutInspectionFeatureRequest
			if err := ctx.ShouldBindJSON(&reqBody); err != nil {
				ctx.String(http.StatusBadRequest, err.Error())
				return
			}
			err := task.SetFeatureList(reqBody.Features)
			if err != nil {
				ctx.String(http.StatusInternalServerError, err.Error())
				return
			}
			ctx.String(http.StatusAccepted, "ok")
		})
		// PATCH /api/v3/inspection/<inspection-id>/features
		router.PATCH("/api/v3/inspection/:inspectionID/features", func(ctx *gin.Context) {
			inspectionID := ctx.Param("inspectionID")
			task := inspectionServer.GetInspection(inspectionID)
			if task == nil {
				ctx.String(http.StatusNotFound, fmt.Sprintf("inspecton %s was not found", inspectionID))
				return
			}
			var reqBody PatchInspectionFeatureRequest
			if err := ctx.ShouldBindJSON(&reqBody); err != nil {
				ctx.String(http.StatusBadRequest, err.Error())
				return
			}
			err := task.UpdateFeatureMap(reqBody.Features)
			if err != nil {
				ctx.String(http.StatusInternalServerError, err.Error())
				return
			}
			ctx.String(http.StatusAccepted, "ok")
		})
		// GET /api/v3/inspection/<inspection-id>/features
		router.GET("/api/v3/inspection/:inspectionID/features", func(ctx *gin.Context) {
			inspectionID := ctx.Param("inspectionID")
			task := inspectionServer.GetInspection(inspectionID)
			if task == nil {
				ctx.String(http.StatusNotFound, fmt.Sprintf("inspecton %s was not found", inspectionID))
				return
			}
			features, err := task.FeatureList()
			if err != nil {
				ctx.String(http.StatusInternalServerError, err.Error())
				return
			}
			ctx.JSON(http.StatusOK, GetInspectionFeatureResponse{
				Features: features,
			})
		})

		router.POST("/api/v3/inspection/:inspectionID/dryrun", func(ctx *gin.Context) {
			inspectionID := ctx.Param("inspectionID")
			currentTask := inspectionServer.GetInspection(inspectionID)
			if currentTask == nil {
				ctx.String(http.StatusNotFound, fmt.Sprintf("inspecton %s was not found", inspectionID))
				return
			}
			var reqBody PostInspectionDryRunRequest
			if err := ctx.ShouldBindJSON(&reqBody); err != nil {
				ctx.String(http.StatusBadRequest, err.Error())
				return
			}
			result, err := currentTask.DryRun(ctx, &inspectioncore_contract.InspectionRequest{
				Values: reqBody,
			})
			if err != nil {
				ctx.String(http.StatusInternalServerError, err.Error())
				return
			}
			ctx.JSON(http.StatusOK, result)
		})

		router.POST("/api/v3/inspection/:inspectionID/run", func(ctx *gin.Context) {
			inspectionID := ctx.Param("inspectionID")
			currentTask := inspectionServer.GetInspection(inspectionID)
			if currentTask == nil {
				ctx.String(http.StatusNotFound, fmt.Sprintf("inspecton %s was not found", inspectionID))
				return
			}
			var reqBody PostInspectionDryRunRequest
			if err := ctx.ShouldBindJSON(&reqBody); err != nil {
				ctx.String(http.StatusBadRequest, err.Error())
				return
			}
			err := currentTask.Run(ctx, &inspectioncore_contract.InspectionRequest{
				Values: reqBody,
			})
			if err != nil {
				ctx.String(http.StatusInternalServerError, err.Error())
				return
			}
			ctx.String(http.StatusAccepted, "ok")
		})

		router.POST("/api/v3/inspection/:inspectionID/cancel", func(ctx *gin.Context) {
			inspectionID := ctx.Param("inspectionID")
			currentTask := inspectionServer.GetInspection(inspectionID)
			if currentTask == nil {
				ctx.String(http.StatusNotFound, fmt.Sprintf("inspecton %s was not found", inspectionID))
				return
			}
			err := currentTask.Cancel()
			if err != nil {
				ctx.String(http.StatusBadRequest, err.Error())
				return
			}
			ctx.String(http.StatusOK, "ok")
		})
	}

	// GET /api/v3/inspection - available in both viewer and non-viewer mode.
	// Returns inspections from running tasks and/or .khi files in the data destination folder.
	// In viewer mode only file-based inspections are returned.
	router.GET("/api/v3/inspection", func(ctx *gin.Context) {
		responseInspections := map[string]SerializedMetadata{}
		if !serverConfig.ViewerMode {
			inspections := inspectionServer.GetAllRunners()
			for _, inspection := range inspections {
				if inspection.Started() {
					md, err := inspection.GetCurrentMetadata()
					if err != nil {
						ctx.String(http.StatusInternalServerError, err.Error())
						return
					}

					m, err := inspectionmetadata.GetSerializableSubsetMapFromMetadataSet(md, filter.NewEnabledFilter(inspectionmetadata.LabelKeyIncludedInTaskListFlag, false))
					if err != nil {
						ctx.String(http.StatusInternalServerError, err.Error())
						return
					}
					responseInspections[inspection.ID] = m
				}
			}
		}
		if serverConfig.DataDestinationFolder != "" {
			fileInspections, err := listInspectionsFromDataDestination(serverConfig.DataDestinationFolder)
			if err != nil {
				slog.Debug("Listing .khi files in data destination failed", "folder", serverConfig.DataDestinationFolder, "error", err)
			} else {
				for id, meta := range fileInspections {
					if _, exists := responseInspections[id]; !exists {
						responseInspections[id] = meta
					}
				}
			}
		}

		ctx.JSON(http.StatusOK, &GetInspectionsResponse{
			Inspections: responseInspections,
			ServerStat: &ServerStat{
				TotalMemoryAvailable: serverConfig.ResourceMonitor.GetUsedMemory(),
			},
		})
	})

	// GET /api/v3/inspection/:inspectionID/metadata - available in both modes; falls back to file-based inspection when no runner.
	router.GET("/api/v3/inspection/:inspectionID/metadata", func(ctx *gin.Context) {
		inspectionID := ctx.Param("inspectionID")
		currentTask := inspectionServer.GetInspection(inspectionID)
		if currentTask != nil {
			result, err := currentTask.Metadata()
			if err != nil {
				ctx.String(http.StatusBadRequest, err.Error())
				return
			}
			ctx.JSON(http.StatusOK, result)
			return
		}
		// Fall back to file-based inspection (viewer mode or completed .khi file).
		if serverConfig.DataDestinationFolder != "" {
			meta, err := metadataForFileBasedInspection(serverConfig.DataDestinationFolder, inspectionID)
			if err == nil {
				ctx.JSON(http.StatusOK, meta)
				return
			}
		}
		ctx.String(http.StatusNotFound, fmt.Sprintf("inspection %s was not found", inspectionID))
	})

	// GET /api/v3/inspection/:inspectionID/data - available in both modes; falls back to serving .khi file when no runner.
	router.GET("/api/v3/inspection/:inspectionID/data", func(ctx *gin.Context) {
		inspectionID := ctx.Param("inspectionID")
		currentTask := inspectionServer.GetInspection(inspectionID)
		if currentTask != nil {
			var rangeStart int64
			var maxSize int64 = math.MaxInt64
			startQueryStr := ctx.Query("start")
			maxSizeQueryStr := ctx.Query("maxSize")
			if startQueryStr != "" {
				var err error
				rangeStart, err = strconv.ParseInt(startQueryStr, 10, 64)
				if err != nil {
					ctx.String(http.StatusBadRequest, err.Error())
					return
				}
			}
			if maxSizeQueryStr != "" {
				var err error
				maxSize, err = strconv.ParseInt(maxSizeQueryStr, 10, 64)
				if err != nil {
					ctx.String(http.StatusBadRequest, err.Error())
					return
				}
			}
			result, err := currentTask.Result()
			if err != nil {
				ctx.String(http.StatusBadRequest, err.Error())
				return
			}
			inspectionDataReader, err := result.ResultStore.GetRangeReader(rangeStart, maxSize)
			if err != nil {
				ctx.String(http.StatusInternalServerError, err.Error())
				return
			}
			defer inspectionDataReader.Close()
			fileSize, err := result.ResultStore.GetInspectionResultSizeInBytes()
			if err != nil {
				ctx.String(http.StatusInternalServerError, err.Error())
				return
			}
			ctx.DataFromReader(http.StatusOK, min(maxSize, int64(fileSize)-rangeStart), "application/octet-stream", inspectionDataReader, map[string]string{})
			return
		}
		// Fall back to file-based inspection: stream .khi file.
		if serverConfig.DataDestinationFolder != "" {
			var rangeStart int64
			var maxSize int64 = math.MaxInt64
			if startQueryStr := ctx.Query("start"); startQueryStr != "" {
				var err error
				rangeStart, err = strconv.ParseInt(startQueryStr, 10, 64)
				if err != nil {
					ctx.String(http.StatusBadRequest, err.Error())
					return
				}
			}
			if maxSizeQueryStr := ctx.Query("maxSize"); maxSizeQueryStr != "" {
				var err error
				maxSize, err = strconv.ParseInt(maxSizeQueryStr, 10, 64)
				if err != nil {
					ctx.String(http.StatusBadRequest, err.Error())
					return
				}
			}
			reader, contentLength, err := openInspectionDataFileRange(serverConfig.DataDestinationFolder, inspectionID, rangeStart, maxSize)
			if err == nil {
				defer reader.Close()
				ctx.DataFromReader(http.StatusOK, contentLength, "application/octet-stream", reader, map[string]string{})
				return
			}
		}
		ctx.String(http.StatusNotFound, fmt.Sprintf("inspection %s was not found", inspectionID))
	})

	if !serverConfig.ViewerMode {
		router.GET("/api/v3/popup", func(ctx *gin.Context) {
			currentPopup := popup.Instance.GetCurrentPopup()
			if currentPopup == nil {
				ctx.String(http.StatusOK, "")
				return
			}
			ctx.JSON(http.StatusOK, currentPopup)
		})

		router.POST("/api/v3/popup/validate", func(ctx *gin.Context) {
			request := &popup.PopupAnswerResponse{}
			if err := ctx.ShouldBindJSON(request); err != nil {
				ctx.String(http.StatusBadRequest, err.Error())
				return
			}
			result, err := popup.Instance.Validate(request)
			if errors.Is(err, popup.NoCurrentPopup) {
				ctx.String(http.StatusNotFound, err.Error())
				return
			}
			if errors.Is(err, popup.CurrentPopupIsntMatchingWithGivenId) {
				ctx.String(http.StatusBadRequest, err.Error())
				return
			}
			if err != nil {
				ctx.String(http.StatusInternalServerError, err.Error())
				return
			}
			ctx.JSON(http.StatusOK, result)
		})

		router.POST("/api/v3/popup/answer", func(ctx *gin.Context) {
			request := &popup.PopupAnswerResponse{}
			if err := ctx.ShouldBindJSON(request); err != nil {
				ctx.String(http.StatusBadRequest, err.Error())
				return
			}
			err := popup.Instance.Answer(request)
			if errors.Is(err, popup.NoCurrentPopup) {
				ctx.String(http.StatusNotFound, err.Error())
				return
			}
			if errors.Is(err, popup.CurrentPopupIsntMatchingWithGivenId) {
				ctx.String(http.StatusBadRequest, err.Error())
				return
			}
			if err != nil {
				ctx.String(http.StatusInternalServerError, err.Error())
				return
			}
			ctx.String(http.StatusOK, "")
		})

		router.POST("/api/v3/upload", func(ctx *gin.Context) {
			localUploadFileStoreProvider, convertible := serverConfig.UploadFileStore.StoreProvider.(*upload.LocalUploadFileStoreProvider)
			if !convertible {
				ctx.String(http.StatusBadRequest, "invalid operation. Current UploadFileStore.StoreProvider is not supporting to be written directly")
				return
			}
			file, err := ctx.FormFile("file")
			if err != nil {
				ctx.String(http.StatusBadRequest, err.Error())
				return
			}

			id := ctx.Request.FormValue("upload-token-id")
			if id == "" {
				ctx.String(http.StatusBadRequest, "missing upload-token-id")
				return
			}

			token := &upload.DirectUploadToken{ID: id}
			if parameters.Server.MaxUploadFileSizeInBytes != nil && *parameters.Server.MaxUploadFileSizeInBytes < int(file.Size) {
				ctx.String(http.StatusBadRequest, fmt.Sprintf("file size exceeds the limit (%d bytes)", *parameters.Server.MaxUploadFileSizeInBytes))
				return
			}

			err = serverConfig.UploadFileStore.SetResultOnStartingUpload(token)
			if err != nil {
				ctx.String(http.StatusInternalServerError, err.Error())
				return
			}

			multipart, err := file.Open()
			if err != nil {
				ctx.String(http.StatusBadRequest, err.Error())
				return
			}
			defer multipart.Close()

			err = localUploadFileStoreProvider.Write(token, multipart)
			if err != nil {
				serverConfig.UploadFileStore.SetResultOnCompletedUpload(token, err)
				ctx.String(http.StatusInternalServerError, err.Error())
				return
			}
			serverConfig.UploadFileStore.SetResultOnCompletedUpload(token, nil)

			ctx.String(http.StatusOK, "")
		})
	}
	return engine
}

// listInspectionsFromDataDestination returns a map of inspection ID to serialized metadata for each .khi file in the given folder.
// The inspection ID is the filename without the ".khi" extension. Running inspections take precedence over file-based entries when merging.
func listInspectionsFromDataDestination(dataDestinationFolder string) (map[string]SerializedMetadata, error) {
	pattern := filepath.Join(dataDestinationFolder, "*.khi")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	result := make(map[string]SerializedMetadata, len(matches))
	for _, p := range matches {
		base := filepath.Base(p)
		if !strings.HasSuffix(base, ".khi") {
			continue
		}
		inspectionID := strings.TrimSuffix(base, ".khi")
		if inspectionID == "" {
			continue
		}
		var fileSize int
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			fileSize = int(info.Size())
		}
		suggestedFilename := base
		result[inspectionID] = SerializedMetadata{
			"header": map[string]any{
				"inspectionType":         "",
				"inspectionName":         inspectionID,
				"inspectionTypeIconPath": "",
				"startTimeUnixSeconds":   int64(0),
				"endTimeUnixSeconds":     int64(0),
				"inspectTimeUnixSeconds": int64(0),
				"suggestedFilename":      suggestedFilename,
				"fileSize":               fileSize,
			},
			"progress": map[string]any{
				"phase": inspectionmetadata.TaskPhaseDone,
				"totalProgress": map[string]any{
					"id":            "Total",
					"label":         "Total",
					"message":      "",
					"percentage":   float32(100),
					"indeterminate": false,
				},
				"progresses": []any{},
			},
			"error": map[string]any{
				"errorMessages": []any{},
			},
		}
	}
	return result, nil
}

// metadataForFileBasedInspection returns run-result metadata for a .khi file (minimal stub for viewer mode).
func metadataForFileBasedInspection(dataDestinationFolder, inspectionID string) (SerializedMetadata, error) {
	base := inspectionID + ".khi"
	filePath := filepath.Join(dataDestinationFolder, base)
	info, err := os.Stat(filePath)
	if err != nil || !info.Mode().IsRegular() {
		return nil, os.ErrNotExist
	}
	return SerializedMetadata{
		"header": map[string]any{
			"inspectionType":         "",
			"inspectionName":         inspectionID,
			"inspectionTypeIconPath": "",
			"startTimeUnixSeconds":   int64(0),
			"endTimeUnixSeconds":     int64(0),
			"inspectTimeUnixSeconds": int64(0),
			"suggestedFilename":      base,
			"fileSize":               int(info.Size()),
		},
		"query": []any{},
		"plan":  map[string]any{"plan": ""},
		"log":   []any{},
		"error": map[string]any{"errorMessages": []any{}},
	}, nil
}

// openInspectionDataFileRange opens a range of the .khi file for the given inspection ID.
func openInspectionDataFileRange(dataDestinationFolder, inspectionID string, start, maxLength int64) (io.ReadCloser, int64, error) {
	filePath := filepath.Join(dataDestinationFolder, inspectionID+".khi")
	if strings.Contains(inspectionID, "/") {
		return nil, 0, os.ErrNotExist
	}
	info, err := os.Stat(filePath)
	if err != nil || !info.Mode().IsRegular() {
		return nil, 0, os.ErrNotExist
	}
	f, err := os.Open(filePath)
	if err != nil {
		return nil, 0, err
	}
	fileSize := info.Size()
	contentLength := min(maxLength, fileSize-start)
	if contentLength < 0 {
		contentLength = 0
	}
	sectionReader := io.NewSectionReader(f, start, contentLength)
	return struct {
		io.Reader
		io.Closer
	}{
		sectionReader,
		f,
	}, contentLength, nil
}
