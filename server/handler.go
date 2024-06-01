package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dangjinghao/uclipboard/model"
	"github.com/dangjinghao/uclipboard/server/core"
	"github.com/gin-gonic/gin"
)

func HandlerPush(conf *model.Conf) func(ctx *gin.Context) {
	logger := model.NewModuleLogger("HandlerPush")

	return func(ctx *gin.Context) {
		clipboardData := model.NewClipoardWithDefault()
		if err := ctx.BindJSON(&clipboardData); err != nil {
			logger.Tracef("BindJSON error: %v", err)
			ctx.JSON(http.StatusBadRequest, model.NewDefaultServeRes("request is invalid.", nil))
			return
		}
		if clipboardData.Content == "" {
			ctx.JSON(http.StatusBadRequest, model.NewDefaultServeRes("content is empty", nil))
			return
		}
		if err := core.AddClipboardRecord(clipboardData); err != nil {
			logger.Tracef("AddClipboardRecord error: %v", err)
			ctx.JSON(http.StatusInternalServerError, model.NewDefaultServeRes("add clipboard record error", nil))
			return
		}
		ctx.JSON(http.StatusOK, model.NewDefaultServeRes("", nil))
	}
}
func HandlerPull(conf *model.Conf) func(ctx *gin.Context) {
	logger := model.NewModuleLogger("HandlerPull")

	return func(ctx *gin.Context) {
		clipboardArr, err := core.QueryLatestClipboardRecord(conf.Server.PullHistorySize)
		if err != nil {
			logger.Tracef("GetLatestClipboardRecord error: %v", err)
			ctx.JSON(http.StatusInternalServerError, model.NewDefaultServeRes("get latest clipboard record error", nil))
			return
		}
		clipboardsBytes, err := json.Marshal(clipboardArr)
		if err != nil {
			logger.Tracef("Marshal clipboardArr error: %v", err)
			ctx.JSON(http.StatusInternalServerError, model.NewDefaultServeRes("marshal clipboardArr error", nil))
			return
		}

		ctx.JSON(http.StatusOK, model.NewDefaultServeRes("", clipboardsBytes))
	}
}

func HandlerUpload(conf *model.Conf) func(ctx *gin.Context) {
	logger := model.NewModuleLogger("HandlerUpload")
	return func(ctx *gin.Context) {
		file, err := ctx.FormFile("file")
		if err != nil {
			ctx.JSON(http.StatusBadRequest, model.NewDefaultServeRes("can't read file from request", nil))
			return
		}
		lifetime := ctx.Query("lifetime")
		lifetimeMS, err := core.ConvertLifetime(lifetime, conf.Runtime.DefaultFileLifeMS)
		if err != nil {
			logger.Debugf("ConvertLifetime error: %v", err)
			ctx.JSON(http.StatusBadRequest, model.NewDefaultServeRes("invalid lifetime", nil))
			return
		}
		logger.Tracef("lifetime: %vms", lifetimeMS)

		hostname := ctx.Request.Header.Get("hostname")
		if hostname == "" {
			hostname = "unknown"
		}
		logger.Debugf("uploader's hostname is: %s", hostname)

		if file.Filename == "" {
			// It would not be happened on uclipboard client
			file.Filename = model.RandString(8)
			logger.Warnf("Filename is empty, generate a random filename:%v", file.Filename)
		}
		// prepare to download file
		fileMetadata := model.NewFileMetadataWithDefault()
		fileMetadata.FileName = file.Filename
		fileMetadata.TmpPath = fmt.Sprintf("%s_%s", strconv.FormatInt(fileMetadata.CreatedTs, 10), file.Filename)
		fileMetadata.ExpireTs = lifetimeMS + fileMetadata.CreatedTs
		logger.Debugf("Upload file metadata is: %v", fileMetadata)

		// save file to tmp directory and get the path to save in db
		filePath := filepath.Join(conf.Server.TmpPath, fileMetadata.TmpPath)
		if err := ctx.SaveUploadedFile(file, filePath); err != nil {
			logger.Warnf("SaveUploadedFile error: %v", err)
			ctx.JSON(http.StatusInternalServerError, model.NewDefaultServeRes("save file error", nil))
			return
		}

		// FIXME: I don't know, maybe I should use transaction
		// When one of the following operations fails, the saved file should be deleted
		// And at that time, both of the tables are not synchronized

		// save file metadata to db
		fileId, err := core.AddFileMetadataRecord(fileMetadata)
		if err != nil {
			logger.Tracef("AddFileMetadataRecord error: %v", err)
			ctx.JSON(http.StatusInternalServerError, model.NewDefaultServeRes("add file metadata record error", nil))
			return
		}
		logger.Debugf("The new file id is %v", fileId)

		// save clipboard record to db
		newClipboardRecord := model.NewClipoardWithDefault()
		newClipboardRecord.Content = fmt.Sprintf("%s@%d", fileMetadata.FileName, fileId) // add fileid to clipboard record
		newClipboardRecord.Hostname = hostname
		newClipboardRecord.ContentType = "binary"
		logger.Tracef("Upload binary file clipboard record: %v", newClipboardRecord)
		if err := core.AddClipboardRecord(newClipboardRecord); err != nil {
			logger.Tracef("AddClipboardRecord error: %v", err)
			ctx.JSON(http.StatusInternalServerError, model.NewDefaultServeRes("add clipboard record error", nil))
			return
		}

		responseData, err := json.Marshal(gin.H{"file_id": fileId, "file_name": fileMetadata.FileName,
			"life_time": lifetimeMS / 1000}) // ms -> s
		if err != nil {
			logger.Tracef("Marshal response data error: %v", err)
			ctx.JSON(http.StatusInternalServerError, model.NewDefaultServeRes("marshal response data error", nil))
			return
		}

		ctx.JSON(http.StatusOK, model.NewDefaultServeRes("", responseData))
	}
}

func HandlerDownload(conf *model.Conf) func(ctx *gin.Context) {
	logger := model.NewModuleLogger("HandlerDownload")

	return func(ctx *gin.Context) {
		logger.Debugf("request download raw filename paramater: %v", ctx.Param("filename"))

		fileName := ctx.Param("filename")[1:] // skip '/' in '/xxx'
		metadata := model.NewFileMetadataWithDefault()
		if fileName == "" {
			// download latest binary file
			if err := core.GetFileMetadataLatestRecord(metadata); err != nil {
				if err == sql.ErrNoRows {
					ctx.JSON(http.StatusNotFound, model.NewDefaultServeRes("file not found", nil))
					return
				}

				logger.Debugf("GetFileMetadataLatestRecord error: %v", err)
				ctx.JSON(http.StatusInternalServerError, model.NewDefaultServeRes("get the latest file metadata record error", nil))
				return
			}

			logger.Debugf("Get the latest file metadata record: %v", metadata)
		} else {
			if strings.Contains(fileName, "@") {
				// download by id

				// get the id number starts after @
				id := core.ExtractFileId(fileName, "@")
				if id == 0 {
					ctx.JSON(http.StatusInternalServerError, model.NewDefaultServeRes("invalid format of file id ", nil))
					return
				}
				metadata.Id = id
				logger.Debugf("download by id: %v", metadata.Id)
			} else {
				// download by name
				metadata.FileName = fileName
				logger.Debugf("download by name: %s", metadata.FileName)
			}
			
			err := core.GetFileMetadataRecordByIdOrName(metadata)
			if err != nil {
				if err == sql.ErrNoRows {
					ctx.JSON(http.StatusNotFound, model.NewDefaultServeRes("file not found", nil))
					return
				}
				ctx.JSON(http.StatusInternalServerError, model.NewDefaultServeRes("get file metadata record error: "+err.Error(), nil))
				return
			}

		}

		fullPath := path.Join(conf.Server.TmpPath, metadata.TmpPath)
		logger.Debugf("Required file full path: %s", fullPath)
		// set file name in header
		ctx.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, metadata.FileName))
		ctx.File(fullPath)
	}
}

func HandlerHistory(c *model.Conf) func(ctx *gin.Context) {
	logger := model.NewModuleLogger("HandlerHistory")
	return func(ctx *gin.Context) {
		page := ctx.Query("page")
		if page == "" {
			page = "1"
		}
		pageInt, err := strconv.Atoi(page)
		if err != nil {
			ctx.JSON(http.StatusBadRequest, model.NewDefaultServeRes("invalid page number", nil))
			return
		}
		logger.Tracef("Request clipboard history page: %v", pageInt)
		clipboards, err := core.QueryClipboardHistory(c, pageInt)
		if err != nil {
			logger.Tracef("QueryClipboardHistory error: %v", err)
			ctx.JSON(http.StatusInternalServerError, model.NewDefaultServeRes("query clipboard history error", nil))
			return
		}
		clipboardsBytes, err := json.Marshal(clipboards)
		if err != nil {
			logger.Tracef("Marshal clipboards error: %v", err)
			ctx.JSON(http.StatusInternalServerError, model.NewDefaultServeRes("marshal clipboards error", nil))
			return
		}

		ctx.JSON(http.StatusOK, model.NewDefaultServeRes("", clipboardsBytes))
	}
}

func HandlerPublicShare(c *model.Conf) func(ctx *gin.Context) {
	// TODO share binary file to public
	return func(ctx *gin.Context) {
	}
}
