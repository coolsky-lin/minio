// Copyright (c) 2015-2021 MinIO, Inc.
//
// This file is part of MinIO Object Storage stack
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package cmd

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/user"
	"path"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio/internal/grid"
	"github.com/tinylib/msgp/msgp"

	jwtreq "github.com/golang-jwt/jwt/v4/request"
	"github.com/minio/madmin-go/v3"
	"github.com/minio/minio/internal/config"
	xhttp "github.com/minio/minio/internal/http"
	xioutil "github.com/minio/minio/internal/ioutil"
	xjwt "github.com/minio/minio/internal/jwt"
	"github.com/minio/minio/internal/logger"
	"github.com/minio/mux"
	xnet "github.com/minio/pkg/v2/net"
)

var errDiskStale = errors.New("drive stale")

// To abstract a disk over network.
type storageRESTServer struct {
	poolIndex, setIndex, diskIndex int
}

func (s *storageRESTServer) getStorage() StorageAPI {
	globalLocalDrivesMu.RLock()
	defer globalLocalDrivesMu.RUnlock()
	return globalLocalSetDrives[s.poolIndex][s.setIndex][s.diskIndex]
}

func (s *storageRESTServer) writeErrorResponse(w http.ResponseWriter, err error) {
	err = unwrapAll(err)
	switch err {
	case errDiskStale:
		w.WriteHeader(http.StatusPreconditionFailed)
	case errFileNotFound, errFileVersionNotFound:
		w.WriteHeader(http.StatusNotFound)
	case errInvalidAccessKeyID, errAccessKeyDisabled, errNoAuthToken, errMalformedAuth, errAuthentication, errSkewedAuthTime:
		w.WriteHeader(http.StatusUnauthorized)
	case context.Canceled, context.DeadlineExceeded:
		w.WriteHeader(499)
	default:
		w.WriteHeader(http.StatusForbidden)
	}
	w.Write([]byte(err.Error()))
}

// DefaultSkewTime - skew time is 15 minutes between minio peers.
const DefaultSkewTime = 15 * time.Minute

// Authenticates storage client's requests and validates for skewed time.
func storageServerRequestValidate(r *http.Request) error {
	token, err := jwtreq.AuthorizationHeaderExtractor.ExtractToken(r)
	if err != nil {
		if err == jwtreq.ErrNoTokenInRequest {
			return errNoAuthToken
		}
		return errMalformedAuth
	}

	claims := xjwt.NewStandardClaims()
	if err = xjwt.ParseWithStandardClaims(token, claims, []byte(globalActiveCred.SecretKey)); err != nil {
		return errAuthentication
	}

	owner := claims.AccessKey == globalActiveCred.AccessKey || claims.Subject == globalActiveCred.AccessKey
	if !owner {
		return errAuthentication
	}

	if claims.Audience != r.URL.RawQuery {
		return errAuthentication
	}

	requestTimeStr := r.Header.Get("X-Minio-Time")
	requestTime, err := time.Parse(time.RFC3339, requestTimeStr)
	if err != nil {
		return errMalformedAuth
	}
	utcNow := UTCNow()
	delta := requestTime.Sub(utcNow)
	if delta < 0 {
		delta *= -1
	}
	if delta > DefaultSkewTime {
		return errSkewedAuthTime
	}

	return nil
}

// IsAuthValid - To authenticate and verify the time difference.
func (s *storageRESTServer) IsAuthValid(w http.ResponseWriter, r *http.Request) bool {
	if s.getStorage() == nil {
		s.writeErrorResponse(w, errDiskNotFound)
		return false
	}

	if err := storageServerRequestValidate(r); err != nil {
		s.writeErrorResponse(w, err)
		return false
	}

	return true
}

// IsValid - To authenticate and check if the disk-id in the request corresponds to the underlying disk.
func (s *storageRESTServer) IsValid(w http.ResponseWriter, r *http.Request) bool {
	if !s.IsAuthValid(w, r) {
		return false
	}

	if err := r.ParseForm(); err != nil {
		s.writeErrorResponse(w, err)
		return false
	}

	diskID := r.Form.Get(storageRESTDiskID)
	if diskID == "" {
		// Request sent empty disk-id, we allow the request
		// as the peer might be coming up and trying to read format.json
		// or create format.json
		return true
	}

	storedDiskID, err := s.getStorage().GetDiskID()
	if err != nil {
		s.writeErrorResponse(w, err)
		return false
	}

	if diskID != storedDiskID {
		s.writeErrorResponse(w, errDiskStale)
		return false
	}

	// If format.json is available and request sent the right disk-id, we allow the request
	return true
}

// checkID - check if the disk-id in the request corresponds to the underlying disk.
func (s *storageRESTServer) checkID(wantID string) bool {
	if s.getStorage() == nil {
		return false
	}
	if wantID == "" {
		// Request sent empty disk-id, we allow the request
		// as the peer might be coming up and trying to read format.json
		// or create format.json
		return true
	}

	storedDiskID, err := s.getStorage().GetDiskID()
	if err != nil {
		return false
	}

	return wantID == storedDiskID
}

// HealthHandler handler checks if disk is stale
func (s *storageRESTServer) HealthHandler(w http.ResponseWriter, r *http.Request) {
	s.IsValid(w, r)
}

// DiskInfo types.
// DiskInfo.Metrics elements are shared, so we cannot reuse.
var storageDiskInfoHandler = grid.NewSingleHandler[*grid.MSS, *DiskInfo](grid.HandlerDiskInfo, grid.NewMSS, func() *DiskInfo { return &DiskInfo{} }).WithSharedResponse()

// DiskInfoHandler - returns disk info.
func (s *storageRESTServer) DiskInfoHandler(params *grid.MSS) (*DiskInfo, *grid.RemoteErr) {
	if !s.checkID(params.Get(storageRESTDiskID)) {
		return nil, grid.NewRemoteErr(errDiskNotFound)
	}
	withMetrics := params.Get(storageRESTMetrics) == "true"
	info, err := s.getStorage().DiskInfo(context.Background(), withMetrics)
	if err != nil {
		info.Error = err.Error()
	}
	return &info, nil
}

// scanner rpc handler.
var storageNSScannerHandler = grid.NewStream[*nsScannerOptions, grid.NoPayload, *nsScannerResp](grid.HandlerNSScanner,
	func() *nsScannerOptions { return &nsScannerOptions{} },
	nil,
	func() *nsScannerResp { return &nsScannerResp{} })

func (s *storageRESTServer) NSScannerHandler(ctx context.Context, params *nsScannerOptions, out chan<- *nsScannerResp) *grid.RemoteErr {
	if !s.checkID(params.DiskID) {
		return grid.NewRemoteErr(errDiskNotFound)
	}
	if params.Cache == nil {
		return grid.NewRemoteErrString("NSScannerHandler: provided cache is nil")
	}

	// Collect updates, stream them before the full cache is sent.
	updates := make(chan dataUsageEntry, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for update := range updates {
			resp := storageNSScannerHandler.NewResponse()
			resp.Update = &update
			out <- resp
		}
	}()
	ui, err := s.getStorage().NSScanner(ctx, *params.Cache, updates, madmin.HealScanMode(params.ScanMode))
	wg.Wait()
	if err != nil {
		return grid.NewRemoteErr(err)
	}
	// Send final response.
	resp := storageNSScannerHandler.NewResponse()
	resp.Final = &ui
	out <- resp
	return nil
}

// MakeVolHandler - make a volume.
func (s *storageRESTServer) MakeVolHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		return
	}
	volume := r.Form.Get(storageRESTVolume)
	err := s.getStorage().MakeVol(r.Context(), volume)
	if err != nil {
		s.writeErrorResponse(w, err)
	}
}

// MakeVolBulkHandler - create multiple volumes as a bulk operation.
func (s *storageRESTServer) MakeVolBulkHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		return
	}
	volumes := strings.Split(r.Form.Get(storageRESTVolumes), ",")
	err := s.getStorage().MakeVolBulk(r.Context(), volumes...)
	if err != nil {
		s.writeErrorResponse(w, err)
	}
}

// ListVolsHandler - list volumes.
func (s *storageRESTServer) ListVolsHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		return
	}
	infos, err := s.getStorage().ListVols(r.Context())
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}
	logger.LogIf(r.Context(), msgp.Encode(w, VolsInfo(infos)))
}

// statvol types.
var storageStatVolHandler = grid.NewSingleHandler[*grid.MSS, *VolInfo](grid.HandlerStatVol, grid.NewMSS, func() *VolInfo { return &VolInfo{} })

// StatVolHandler - stat a volume.
func (s *storageRESTServer) StatVolHandler(params *grid.MSS) (*VolInfo, *grid.RemoteErr) {
	if !s.checkID(params.Get(storageRESTDiskID)) {
		return nil, grid.NewRemoteErr(errDiskNotFound)
	}
	info, err := s.getStorage().StatVol(context.Background(), params.Get(storageRESTVolume))
	if err != nil {
		return nil, grid.NewRemoteErr(err)
	}
	return &info, nil
}

// DeleteVolHandler - delete a volume.
func (s *storageRESTServer) DeleteVolHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		return
	}
	volume := r.Form.Get(storageRESTVolume)
	forceDelete := r.Form.Get(storageRESTForceDelete) == "true"
	err := s.getStorage().DeleteVol(r.Context(), volume, forceDelete)
	if err != nil {
		s.writeErrorResponse(w, err)
	}
}

// AppendFileHandler - append data from the request to the file specified.
func (s *storageRESTServer) AppendFileHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		return
	}
	volume := r.Form.Get(storageRESTVolume)
	filePath := r.Form.Get(storageRESTFilePath)

	buf := make([]byte, r.ContentLength)
	_, err := io.ReadFull(r.Body, buf)
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}
	err = s.getStorage().AppendFile(r.Context(), volume, filePath, buf)
	if err != nil {
		s.writeErrorResponse(w, err)
	}
}

// CreateFileHandler - copy the contents from the request.
func (s *storageRESTServer) CreateFileHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		return
	}
	volume := r.Form.Get(storageRESTVolume)
	filePath := r.Form.Get(storageRESTFilePath)

	fileSizeStr := r.Form.Get(storageRESTLength)
	fileSize, err := strconv.Atoi(fileSizeStr)
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}

	done, body := keepHTTPReqResponseAlive(w, r)
	done(s.getStorage().CreateFile(r.Context(), volume, filePath, int64(fileSize), body))
}

var storageDeleteVersionHandler = grid.NewSingleHandler[*DeleteVersionHandlerParams, grid.NoPayload](grid.HandlerDeleteVersion, func() *DeleteVersionHandlerParams {
	return &DeleteVersionHandlerParams{}
}, grid.NewNoPayload)

// DeleteVersionHandler delete updated metadata.
func (s *storageRESTServer) DeleteVersionHandler(p *DeleteVersionHandlerParams) (np grid.NoPayload, gerr *grid.RemoteErr) {
	if !s.checkID(p.DiskID) {
		return np, grid.NewRemoteErr(errDiskNotFound)
	}
	volume := p.Volume
	filePath := p.FilePath
	forceDelMarker := p.ForceDelMarker

	opts := DeleteOptions{}
	err := s.getStorage().DeleteVersion(context.Background(), volume, filePath, p.FI, forceDelMarker, opts)
	return np, grid.NewRemoteErr(err)
}

var storageReadVersionHandler = grid.NewSingleHandler[*grid.MSS, *FileInfo](grid.HandlerReadVersion, grid.NewMSS, func() *FileInfo {
	return &FileInfo{}
})

// ReadVersionHandlerWS read metadata of versionID
func (s *storageRESTServer) ReadVersionHandlerWS(params *grid.MSS) (*FileInfo, *grid.RemoteErr) {
	if !s.checkID(params.Get(storageRESTDiskID)) {
		return nil, grid.NewRemoteErr(errDiskNotFound)
	}
	volume := params.Get(storageRESTVolume)
	filePath := params.Get(storageRESTFilePath)
	versionID := params.Get(storageRESTVersionID)
	readData, err := strconv.ParseBool(params.Get(storageRESTReadData))
	if err != nil {
		return nil, grid.NewRemoteErr(err)
	}

	healing, err := strconv.ParseBool(params.Get(storageRESTHealing))
	if err != nil {
		return nil, grid.NewRemoteErr(err)
	}

	fi, err := s.getStorage().ReadVersion(context.Background(), volume, filePath, versionID, ReadOptions{ReadData: readData, Healing: healing})
	if err != nil {
		return nil, grid.NewRemoteErr(err)
	}
	return &fi, nil
}

// ReadVersionHandler read metadata of versionID
func (s *storageRESTServer) ReadVersionHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		return
	}
	volume := r.Form.Get(storageRESTVolume)
	filePath := r.Form.Get(storageRESTFilePath)
	versionID := r.Form.Get(storageRESTVersionID)
	readData, err := strconv.ParseBool(r.Form.Get(storageRESTReadData))
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}
	healing, err := strconv.ParseBool(r.Form.Get(storageRESTHealing))
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}
	fi, err := s.getStorage().ReadVersion(r.Context(), volume, filePath, versionID, ReadOptions{ReadData: readData, Healing: healing})
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}

	logger.LogIf(r.Context(), msgp.Encode(w, &fi))
}

var storageWriteMetadataHandler = grid.NewSingleHandler[*MetadataHandlerParams, grid.NoPayload](grid.HandlerWriteMetadata, func() *MetadataHandlerParams {
	return &MetadataHandlerParams{}
}, grid.NewNoPayload)

// WriteMetadataHandler rpc handler to write new updated metadata.
func (s *storageRESTServer) WriteMetadataHandler(p *MetadataHandlerParams) (np grid.NoPayload, gerr *grid.RemoteErr) {
	if !s.checkID(p.DiskID) {
		return grid.NewNPErr(errDiskNotFound)
	}
	volume := p.Volume
	filePath := p.FilePath

	err := s.getStorage().WriteMetadata(context.Background(), volume, filePath, p.FI)
	return np, grid.NewRemoteErr(err)
}

var storageUpdateMetadataHandler = grid.NewSingleHandler[*MetadataHandlerParams, grid.NoPayload](grid.HandlerUpdateMetadata, func() *MetadataHandlerParams {
	return &MetadataHandlerParams{}
}, grid.NewNoPayload)

// UpdateMetadataHandler update new updated metadata.
func (s *storageRESTServer) UpdateMetadataHandler(p *MetadataHandlerParams) (grid.NoPayload, *grid.RemoteErr) {
	if !s.checkID(p.DiskID) {
		return grid.NewNPErr(errDiskNotFound)
	}
	volume := p.Volume
	filePath := p.FilePath

	return grid.NewNPErr(s.getStorage().UpdateMetadata(context.Background(), volume, filePath, p.FI, p.UpdateOpts))
}

// WriteAllHandler - write to file all content.
func (s *storageRESTServer) WriteAllHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		return
	}
	volume := r.Form.Get(storageRESTVolume)
	filePath := r.Form.Get(storageRESTFilePath)

	if r.ContentLength < 0 {
		s.writeErrorResponse(w, errInvalidArgument)
		return
	}
	tmp := make([]byte, r.ContentLength)
	_, err := io.ReadFull(r.Body, tmp)
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}
	err = s.getStorage().WriteAll(r.Context(), volume, filePath, tmp)
	if err != nil {
		s.writeErrorResponse(w, err)
	}
}

var storageCheckPartsHandler = grid.NewSingleHandler[*CheckPartsHandlerParams, grid.NoPayload](grid.HandlerCheckParts, func() *CheckPartsHandlerParams {
	return &CheckPartsHandlerParams{}
}, grid.NewNoPayload)

// CheckPartsHandler - check if a file metadata exists.
func (s *storageRESTServer) CheckPartsHandler(p *CheckPartsHandlerParams) (grid.NoPayload, *grid.RemoteErr) {
	if !s.checkID(p.DiskID) {
		return grid.NewNPErr(errDiskNotFound)
	}
	volume := p.Volume
	filePath := p.FilePath
	return grid.NewNPErr(s.getStorage().CheckParts(context.Background(), volume, filePath, p.FI))
}

// ReadAllHandler - read all the contents of a file.
func (s *storageRESTServer) ReadAllHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		return
	}
	volume := r.Form.Get(storageRESTVolume)
	filePath := r.Form.Get(storageRESTFilePath)

	buf, err := s.getStorage().ReadAll(r.Context(), volume, filePath)
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}
	// Reuse after return.
	defer metaDataPoolPut(buf)
	w.Header().Set(xhttp.ContentLength, strconv.Itoa(len(buf)))
	w.Write(buf)
}

// ReadXLHandler - read xl.meta for an object at path.
func (s *storageRESTServer) ReadXLHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		return
	}
	volume := r.Form.Get(storageRESTVolume)
	filePath := r.Form.Get(storageRESTFilePath)
	readData, err := strconv.ParseBool(r.Form.Get(storageRESTReadData))
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}

	rf, err := s.getStorage().ReadXL(r.Context(), volume, filePath, readData)
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}

	logger.LogIf(r.Context(), msgp.Encode(w, &rf))
}

var storageReadXLHandler = grid.NewSingleHandler[*grid.MSS, *RawFileInfo](grid.HandlerReadXL, grid.NewMSS, func() *RawFileInfo {
	return &RawFileInfo{}
})

// ReadXLHandlerWS - read xl.meta for an object at path.
func (s *storageRESTServer) ReadXLHandlerWS(params *grid.MSS) (*RawFileInfo, *grid.RemoteErr) {
	if !s.checkID(params.Get(storageRESTDiskID)) {
		return nil, grid.NewRemoteErr(errDiskNotFound)
	}
	volume := params.Get(storageRESTVolume)
	filePath := params.Get(storageRESTFilePath)
	readData, err := strconv.ParseBool(params.Get(storageRESTReadData))
	if err != nil {
		return nil, grid.NewRemoteErr(err)
	}

	rf, err := s.getStorage().ReadXL(context.Background(), volume, filePath, readData)
	if err != nil {
		return nil, grid.NewRemoteErr(err)
	}

	return &rf, nil
}

// ReadFileHandler - read section of a file.
func (s *storageRESTServer) ReadFileHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		return
	}
	volume := r.Form.Get(storageRESTVolume)
	filePath := r.Form.Get(storageRESTFilePath)
	offset, err := strconv.Atoi(r.Form.Get(storageRESTOffset))
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}
	length, err := strconv.Atoi(r.Form.Get(storageRESTLength))
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}
	if offset < 0 || length < 0 {
		s.writeErrorResponse(w, errInvalidArgument)
		return
	}
	var verifier *BitrotVerifier
	if r.Form.Get(storageRESTBitrotAlgo) != "" {
		hashStr := r.Form.Get(storageRESTBitrotHash)
		var hash []byte
		hash, err = hex.DecodeString(hashStr)
		if err != nil {
			s.writeErrorResponse(w, err)
			return
		}
		verifier = NewBitrotVerifier(BitrotAlgorithmFromString(r.Form.Get(storageRESTBitrotAlgo)), hash)
	}
	buf := make([]byte, length)
	defer metaDataPoolPut(buf) // Reuse if we can.
	_, err = s.getStorage().ReadFile(r.Context(), volume, filePath, int64(offset), buf, verifier)
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}
	w.Header().Set(xhttp.ContentLength, strconv.Itoa(len(buf)))
	w.Write(buf)
}

// ReadFileStreamHandler - read section of a file.
func (s *storageRESTServer) ReadFileStreamHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		return
	}
	volume := r.Form.Get(storageRESTVolume)
	filePath := r.Form.Get(storageRESTFilePath)
	offset, err := strconv.Atoi(r.Form.Get(storageRESTOffset))
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}
	length, err := strconv.Atoi(r.Form.Get(storageRESTLength))
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}

	w.Header().Set(xhttp.ContentLength, strconv.Itoa(length))

	rc, err := s.getStorage().ReadFileStream(r.Context(), volume, filePath, int64(offset), int64(length))
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}
	defer rc.Close()

	rf, ok := w.(io.ReaderFrom)
	if ok && runtime.GOOS != "windows" {
		// Attempt to use splice/sendfile() optimization, A very specific behavior mentioned below is necessary.
		// See https://github.com/golang/go/blob/f7c5cbb82087c55aa82081e931e0142783700ce8/src/net/sendfile_linux.go#L20
		// Windows can lock up with this optimization, so we fall back to regular copy.
		sr, ok := rc.(*sendFileReader)
		if ok {
			_, err = rf.ReadFrom(sr.Reader)
			if !xnet.IsNetworkOrHostDown(err, true) { // do not need to log disconnected clients
				logger.LogIf(r.Context(), err)
			}
			if err == nil || !errors.Is(err, xhttp.ErrNotImplemented) {
				return
			}
		}
	} // Fallback to regular copy

	_, err = xioutil.Copy(w, rc)
	if !xnet.IsNetworkOrHostDown(err, true) { // do not need to log disconnected clients
		logger.LogIf(r.Context(), err)
	}
}

// ListDirHandler - list a directory.
func (s *storageRESTServer) ListDirHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		return
	}
	volume := r.Form.Get(storageRESTVolume)
	dirPath := r.Form.Get(storageRESTDirPath)
	count, err := strconv.Atoi(r.Form.Get(storageRESTCount))
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}

	entries, err := s.getStorage().ListDir(r.Context(), volume, dirPath, count)
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}
	gob.NewEncoder(w).Encode(&entries)
}

var storageDeleteFileHandler = grid.NewSingleHandler[*DeleteFileHandlerParams, grid.NoPayload](grid.HandlerDeleteFile, func() *DeleteFileHandlerParams {
	return &DeleteFileHandlerParams{}
}, grid.NewNoPayload)

// DeleteFileHandler - delete a file.
func (s *storageRESTServer) DeleteFileHandler(p *DeleteFileHandlerParams) (grid.NoPayload, *grid.RemoteErr) {
	if !s.checkID(p.DiskID) {
		return grid.NewNPErr(errDiskNotFound)
	}
	return grid.NewNPErr(s.getStorage().Delete(context.Background(), p.Volume, p.FilePath, p.Opts))
}

// DeleteVersionsErrsResp - collection of delete errors
// for bulk version deletes
type DeleteVersionsErrsResp struct {
	Errs []error
}

// DeleteVersionsHandler - delete a set of a versions.
func (s *storageRESTServer) DeleteVersionsHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		return
	}

	volume := r.Form.Get(storageRESTVolume)
	totalVersions, err := strconv.Atoi(r.Form.Get(storageRESTTotalVersions))
	if err != nil {
		s.writeErrorResponse(w, err)
		return
	}

	versions := make([]FileInfoVersions, totalVersions)
	decoder := msgpNewReader(r.Body)
	defer readMsgpReaderPoolPut(decoder)
	for i := 0; i < totalVersions; i++ {
		dst := &versions[i]
		if err := dst.DecodeMsg(decoder); err != nil {
			s.writeErrorResponse(w, err)
			return
		}
	}

	dErrsResp := &DeleteVersionsErrsResp{Errs: make([]error, totalVersions)}

	setEventStreamHeaders(w)
	encoder := gob.NewEncoder(w)
	done := keepHTTPResponseAlive(w)

	opts := DeleteOptions{}
	errs := s.getStorage().DeleteVersions(r.Context(), volume, versions, opts)
	done(nil)
	for idx := range versions {
		if errs[idx] != nil {
			dErrsResp.Errs[idx] = StorageErr(errs[idx].Error())
		}
	}
	encoder.Encode(dErrsResp)
}

var storageRenameDataHandler = grid.NewSingleHandler[*RenameDataHandlerParams, *RenameDataResp](grid.HandlerRenameData, func() *RenameDataHandlerParams {
	return &RenameDataHandlerParams{}
}, func() *RenameDataResp {
	return &RenameDataResp{}
})

// RenameDataHandler - renames a meta object and data dir to destination.
func (s *storageRESTServer) RenameDataHandler(p *RenameDataHandlerParams) (*RenameDataResp, *grid.RemoteErr) {
	if !s.checkID(p.DiskID) {
		return nil, grid.NewRemoteErr(errDiskNotFound)
	}

	sign, err := s.getStorage().RenameData(context.Background(), p.SrcVolume, p.SrcPath, p.FI, p.DstVolume, p.DstPath, p.Opts)
	resp := &RenameDataResp{
		Signature: sign,
	}
	return resp, grid.NewRemoteErr(err)
}

// RenameFileHandler - rename a file.
func (s *storageRESTServer) RenameFileHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		return
	}
	srcVolume := r.Form.Get(storageRESTSrcVolume)
	srcFilePath := r.Form.Get(storageRESTSrcPath)
	dstVolume := r.Form.Get(storageRESTDstVolume)
	dstFilePath := r.Form.Get(storageRESTDstPath)
	err := s.getStorage().RenameFile(r.Context(), srcVolume, srcFilePath, dstVolume, dstFilePath)
	if err != nil {
		s.writeErrorResponse(w, err)
	}
}

// CleanAbandonedDataHandler - Clean unused data directories.
func (s *storageRESTServer) CleanAbandonedDataHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		return
	}
	volume := r.Form.Get(storageRESTVolume)
	filePath := r.Form.Get(storageRESTFilePath)
	if volume == "" || filePath == "" {
		return // Ignore
	}
	keepHTTPResponseAlive(w)(s.getStorage().CleanAbandonedData(r.Context(), volume, filePath))
}

// closeNotifier is itself a ReadCloser that will notify when either an error occurs or
// the Close() function is called.
type closeNotifier struct {
	rc   io.ReadCloser
	done chan struct{}
}

func (c *closeNotifier) Read(p []byte) (n int, err error) {
	n, err = c.rc.Read(p)
	if err != nil {
		if c.done != nil {
			close(c.done)
			c.done = nil
		}
	}
	return n, err
}

func (c *closeNotifier) Close() error {
	if c.done != nil {
		close(c.done)
		c.done = nil
	}
	return c.rc.Close()
}

// keepHTTPReqResponseAlive can be used to avoid timeouts with long storage
// operations, such as bitrot verification or data usage scanning.
// Every 10 seconds a space character is sent.
// keepHTTPReqResponseAlive will wait for the returned body to be read before starting the ticker.
// The returned function should always be called to release resources.
// An optional error can be sent which will be picked as text only error,
// without its original type by the receiver.
// waitForHTTPResponse should be used to the receiving side.
func keepHTTPReqResponseAlive(w http.ResponseWriter, r *http.Request) (resp func(error), body io.ReadCloser) {
	bodyDoneCh := make(chan struct{})
	doneCh := make(chan error)
	ctx := r.Context()
	go func() {
		canWrite := true
		write := func(b []byte) {
			if canWrite {
				n, err := w.Write(b)
				if err != nil || n != len(b) {
					canWrite = false
				}
			}
		}
		// Wait for body to be read.
		select {
		case <-ctx.Done():
		case <-bodyDoneCh:
		case err := <-doneCh:
			if err != nil {
				write([]byte{1})
				write([]byte(err.Error()))
			} else {
				write([]byte{0})
			}
			close(doneCh)
			return
		}
		defer close(doneCh)
		// Initiate ticker after body has been read.
		ticker := time.NewTicker(time.Second * 10)
		for {
			select {
			case <-ticker.C:
				// Response not ready, write a filler byte.
				write([]byte{32})
				if canWrite {
					w.(http.Flusher).Flush()
				}
			case err := <-doneCh:
				if err != nil {
					write([]byte{1})
					write([]byte(err.Error()))
				} else {
					write([]byte{0})
				}
				ticker.Stop()
				return
			}
		}
	}()
	return func(err error) {
		if doneCh == nil {
			return
		}

		// Indicate we are ready to write.
		doneCh <- err

		// Wait for channel to be closed so we don't race on writes.
		<-doneCh

		// Clear so we can be called multiple times without crashing.
		doneCh = nil
	}, &closeNotifier{rc: r.Body, done: bodyDoneCh}
}

// keepHTTPResponseAlive can be used to avoid timeouts with long storage
// operations, such as bitrot verification or data usage scanning.
// keepHTTPResponseAlive may NOT be used until the request body has been read,
// use keepHTTPReqResponseAlive instead.
// Every 10 seconds a space character is sent.
// The returned function should always be called to release resources.
// An optional error can be sent which will be picked as text only error,
// without its original type by the receiver.
// waitForHTTPResponse should be used to the receiving side.
func keepHTTPResponseAlive(w http.ResponseWriter) func(error) {
	doneCh := make(chan error)
	go func() {
		canWrite := true
		write := func(b []byte) {
			if canWrite {
				n, err := w.Write(b)
				if err != nil || n != len(b) {
					canWrite = false
				}
			}
		}
		defer close(doneCh)
		ticker := time.NewTicker(time.Second * 10)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				// Response not ready, write a filler byte.
				write([]byte{32})
				if canWrite {
					w.(http.Flusher).Flush()
				}
			case err := <-doneCh:
				if err != nil {
					write([]byte{1})
					write([]byte(err.Error()))
				} else {
					write([]byte{0})
				}
				return
			}
		}
	}()
	return func(err error) {
		if doneCh == nil {
			return
		}
		// Indicate we are ready to write.
		doneCh <- err

		// Wait for channel to be closed so we don't race on writes.
		<-doneCh

		// Clear so we can be called multiple times without crashing.
		doneCh = nil
	}
}

// waitForHTTPResponse will wait for responses where keepHTTPResponseAlive
// has been used.
// The returned reader contains the payload.
func waitForHTTPResponse(respBody io.Reader) (io.Reader, error) {
	reader := bufio.NewReader(respBody)
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		// Check if we have a response ready or a filler byte.
		switch b {
		case 0:
			return reader, nil
		case 1:
			errorText, err := io.ReadAll(reader)
			if err != nil {
				return nil, err
			}
			return nil, errors.New(string(errorText))
		case 32:
			continue
		default:
			return nil, fmt.Errorf("unexpected filler byte: %d", b)
		}
	}
}

// httpStreamResponse allows streaming a response, but still send an error.
type httpStreamResponse struct {
	done  chan error
	block chan []byte
	err   error
}

// Write part of the streaming response.
// Note that upstream errors are currently not forwarded, but may be in the future.
func (h *httpStreamResponse) Write(b []byte) (int, error) {
	if len(b) == 0 || h.err != nil {
		// Ignore 0 length blocks
		return 0, h.err
	}
	tmp := make([]byte, len(b))
	copy(tmp, b)
	h.block <- tmp
	return len(b), h.err
}

// CloseWithError will close the stream and return the specified error.
// This can be done several times, but only the first error will be sent.
// After calling this the stream should not be written to.
func (h *httpStreamResponse) CloseWithError(err error) {
	if h.done == nil {
		return
	}
	h.done <- err
	h.err = err
	// Indicates that the response is done.
	<-h.done
	h.done = nil
}

// streamHTTPResponse can be used to avoid timeouts with long storage
// operations, such as bitrot verification or data usage scanning.
// Every 10 seconds a space character is sent.
// The returned function should always be called to release resources.
// An optional error can be sent which will be picked as text only error,
// without its original type by the receiver.
// waitForHTTPStream should be used to the receiving side.
func streamHTTPResponse(w http.ResponseWriter) *httpStreamResponse {
	doneCh := make(chan error)
	blockCh := make(chan []byte)
	h := httpStreamResponse{done: doneCh, block: blockCh}
	go func() {
		canWrite := true
		write := func(b []byte) {
			if canWrite {
				n, err := w.Write(b)
				if err != nil || n != len(b) {
					canWrite = false
				}
			}
		}

		ticker := time.NewTicker(time.Second * 10)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				// Response not ready, write a filler byte.
				write([]byte{32})
				if canWrite {
					w.(http.Flusher).Flush()
				}
			case err := <-doneCh:
				if err != nil {
					write([]byte{1})
					write([]byte(err.Error()))
				} else {
					write([]byte{0})
				}
				close(doneCh)
				return
			case block := <-blockCh:
				var tmp [5]byte
				tmp[0] = 2
				binary.LittleEndian.PutUint32(tmp[1:], uint32(len(block)))
				write(tmp[:])
				write(block)
				if canWrite {
					w.(http.Flusher).Flush()
				}
			}
		}
	}()
	return &h
}

var poolBuf8k = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 8192)
		return &b
	},
}

var poolBuf128k = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 128<<10)
		return b
	},
}

// waitForHTTPStream will wait for responses where
// streamHTTPResponse has been used.
// The returned reader contains the payload and must be closed if no error is returned.
func waitForHTTPStream(respBody io.ReadCloser, w io.Writer) error {
	var tmp [1]byte
	// 8K copy buffer, reused for less allocs...
	bufp := poolBuf8k.Get().(*[]byte)
	buf := *bufp
	defer poolBuf8k.Put(bufp)
	for {
		_, err := io.ReadFull(respBody, tmp[:])
		if err != nil {
			return err
		}
		// Check if we have a response ready or a filler byte.
		switch tmp[0] {
		case 0:
			// 0 is unbuffered, copy the rest.
			_, err := io.CopyBuffer(w, respBody, buf)
			if err == io.EOF {
				return nil
			}
			return err
		case 1:
			errorText, err := io.ReadAll(respBody)
			if err != nil {
				return err
			}
			return errors.New(string(errorText))
		case 2:
			// Block of data
			var tmp [4]byte
			_, err := io.ReadFull(respBody, tmp[:])
			if err != nil {
				return err
			}
			length := binary.LittleEndian.Uint32(tmp[:])
			n, err := io.CopyBuffer(w, io.LimitReader(respBody, int64(length)), buf)
			if err != nil {
				return err
			}
			if n != int64(length) {
				return io.ErrUnexpectedEOF
			}
			continue
		case 32:
			continue
		default:
			return fmt.Errorf("unexpected filler byte: %d", tmp[0])
		}
	}
}

// VerifyFileResp - VerifyFile()'s response.
type VerifyFileResp struct {
	Err error
}

// VerifyFileHandler - Verify all part of file for bitrot errors.
func (s *storageRESTServer) VerifyFileHandler(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		return
	}
	volume := r.Form.Get(storageRESTVolume)
	filePath := r.Form.Get(storageRESTFilePath)

	if r.ContentLength < 0 {
		s.writeErrorResponse(w, errInvalidArgument)
		return
	}

	var fi FileInfo
	if err := msgp.Decode(r.Body, &fi); err != nil {
		s.writeErrorResponse(w, err)
		return
	}

	setEventStreamHeaders(w)
	encoder := gob.NewEncoder(w)
	done := keepHTTPResponseAlive(w)
	err := s.getStorage().VerifyFile(r.Context(), volume, filePath, fi)
	done(nil)
	vresp := &VerifyFileResp{}
	if err != nil {
		vresp.Err = StorageErr(err.Error())
	}
	encoder.Encode(vresp)
}

func checkDiskFatalErrs(errs []error) error {
	// This returns a common error if all errors are
	// same errors, then there is no point starting
	// the server.
	if countErrs(errs, errUnsupportedDisk) == len(errs) {
		return errUnsupportedDisk
	}

	if countErrs(errs, errDiskAccessDenied) == len(errs) {
		return errDiskAccessDenied
	}

	if countErrs(errs, errFileAccessDenied) == len(errs) {
		return errDiskAccessDenied
	}

	if countErrs(errs, errDiskNotDir) == len(errs) {
		return errDiskNotDir
	}

	if countErrs(errs, errFaultyDisk) == len(errs) {
		return errFaultyDisk
	}

	if countErrs(errs, errXLBackend) == len(errs) {
		return errXLBackend
	}

	return nil
}

// A single function to write certain errors to be fatal
// or informative based on the `exit` flag, please look
// at each implementation of error for added hints.
//
// FIXME: This is an unusual function but serves its purpose for
// now, need to revist the overall erroring structure here.
// Do not like it :-(
func logFatalErrs(err error, endpoint Endpoint, exit bool) {
	switch {
	case errors.Is(err, errXLBackend):
		logger.Fatal(config.ErrInvalidXLValue(err), "Unable to initialize backend")
	case errors.Is(err, errUnsupportedDisk):
		var hint string
		if endpoint.URL != nil {
			hint = fmt.Sprintf("Drive '%s' does not support O_DIRECT flags, MinIO erasure coding requires filesystems with O_DIRECT support", endpoint.Path)
		} else {
			hint = "Drives do not support O_DIRECT flags, MinIO erasure coding requires filesystems with O_DIRECT support"
		}
		logger.Fatal(config.ErrUnsupportedBackend(err).Hint(hint), "Unable to initialize backend")
	case errors.Is(err, errDiskNotDir):
		var hint string
		if endpoint.URL != nil {
			hint = fmt.Sprintf("Drive '%s' is not a directory, MinIO erasure coding needs a directory", endpoint.Path)
		} else {
			hint = "Drives are not directories, MinIO erasure coding needs directories"
		}
		logger.Fatal(config.ErrUnableToWriteInBackend(err).Hint(hint), "Unable to initialize backend")
	case errors.Is(err, errDiskAccessDenied):
		// Show a descriptive error with a hint about how to fix it.
		var username string
		if u, err := user.Current(); err == nil {
			username = u.Username
		} else {
			username = "<your-username>"
		}
		var hint string
		if endpoint.URL != nil {
			hint = fmt.Sprintf("Run the following command to add write permissions: `sudo chown -R %s %s && sudo chmod u+rxw %s`",
				username, endpoint.Path, endpoint.Path)
		} else {
			hint = fmt.Sprintf("Run the following command to add write permissions: `sudo chown -R %s. <path> && sudo chmod u+rxw <path>`", username)
		}
		if !exit {
			logger.LogOnceIf(GlobalContext, fmt.Errorf("Drive is not writable %s, %s", endpoint, hint), "log-fatal-errs")
		} else {
			logger.Fatal(config.ErrUnableToWriteInBackend(err).Hint(hint), "Unable to initialize backend")
		}
	case errors.Is(err, errFaultyDisk):
		if !exit {
			logger.LogOnceIf(GlobalContext, fmt.Errorf("Drive is faulty at %s, please replace the drive - drive will be offline", endpoint), "log-fatal-errs")
		} else {
			logger.Fatal(err, "Unable to initialize backend")
		}
	case errors.Is(err, errDiskFull):
		if !exit {
			logger.LogOnceIf(GlobalContext, fmt.Errorf("Drive is already full at %s, incoming I/O will fail - drive will be offline", endpoint), "log-fatal-errs")
		} else {
			logger.Fatal(err, "Unable to initialize backend")
		}
	default:
		if !exit {
			logger.LogOnceIf(GlobalContext, fmt.Errorf("Drive %s returned an unexpected error: %w, please investigate - drive will be offline", endpoint, err), "log-fatal-errs")
		} else {
			logger.Fatal(err, "Unable to initialize backend")
		}
	}
}

// StatInfoFile returns file stat info.
func (s *storageRESTServer) StatInfoFile(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		return
	}
	volume := r.Form.Get(storageRESTVolume)
	filePath := r.Form.Get(storageRESTFilePath)
	glob := r.Form.Get(storageRESTGlob)
	done := keepHTTPResponseAlive(w)
	stats, err := s.getStorage().StatInfoFile(r.Context(), volume, filePath, glob == "true")
	done(err)
	if err != nil {
		return
	}
	for _, si := range stats {
		msgp.Encode(w, &si)
	}
}

// ReadMultiple returns multiple files
func (s *storageRESTServer) ReadMultiple(w http.ResponseWriter, r *http.Request) {
	if !s.IsValid(w, r) {
		return
	}
	rw := streamHTTPResponse(w)
	defer func() {
		if r := recover(); r != nil {
			debug.PrintStack()
			rw.CloseWithError(fmt.Errorf("panic: %v", r))
		}
	}()

	var req ReadMultipleReq
	mr := msgpNewReader(r.Body)
	defer readMsgpReaderPoolPut(mr)
	err := req.DecodeMsg(mr)
	if err != nil {
		rw.CloseWithError(err)
		return
	}

	mw := msgp.NewWriter(rw)
	responses := make(chan ReadMultipleResp, len(req.Files))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for resp := range responses {
			err := resp.EncodeMsg(mw)
			if err != nil {
				rw.CloseWithError(err)
				return
			}
			mw.Flush()
		}
	}()
	err = s.getStorage().ReadMultiple(r.Context(), req, responses)
	wg.Wait()
	rw.CloseWithError(err)
}

// globalLocalSetDrives is used for local drive as well as remote REST
// API caller for other nodes to talk to this node.
//
// Any updates to this must be serialized via globalLocalDrivesMu (locker)
var globalLocalSetDrives [][][]StorageAPI

// registerStorageRESTHandlers - register storage rpc router.
func registerStorageRESTHandlers(router *mux.Router, endpointServerPools EndpointServerPools, gm *grid.Manager) {
	h := func(f http.HandlerFunc) http.HandlerFunc {
		return collectInternodeStats(httpTraceHdrs(f))
	}

	globalLocalSetDrives = make([][][]StorageAPI, len(endpointServerPools))
	for pool := range globalLocalSetDrives {
		globalLocalSetDrives[pool] = make([][]StorageAPI, endpointServerPools[pool].SetCount)
		for set := range globalLocalSetDrives[pool] {
			globalLocalSetDrives[pool][set] = make([]StorageAPI, endpointServerPools[pool].DrivesPerSet)
		}
	}
	for _, serverPool := range endpointServerPools {
		for _, endpoint := range serverPool.Endpoints {
			if !endpoint.IsLocal {
				continue
			}

			server := &storageRESTServer{
				poolIndex: endpoint.PoolIdx,
				setIndex:  endpoint.SetIdx,
				diskIndex: endpoint.DiskIdx,
			}

			subrouter := router.PathPrefix(path.Join(storageRESTPrefix, endpoint.Path)).Subrouter()

			subrouter.Methods(http.MethodPost).Path(storageRESTVersionPrefix + storageRESTMethodHealth).HandlerFunc(h(server.HealthHandler))
			subrouter.Methods(http.MethodPost).Path(storageRESTVersionPrefix + storageRESTMethodMakeVol).HandlerFunc(h(server.MakeVolHandler))
			subrouter.Methods(http.MethodPost).Path(storageRESTVersionPrefix + storageRESTMethodMakeVolBulk).HandlerFunc(h(server.MakeVolBulkHandler))
			subrouter.Methods(http.MethodPost).Path(storageRESTVersionPrefix + storageRESTMethodDeleteVol).HandlerFunc(h(server.DeleteVolHandler))
			subrouter.Methods(http.MethodPost).Path(storageRESTVersionPrefix + storageRESTMethodListVols).HandlerFunc(h(server.ListVolsHandler))

			subrouter.Methods(http.MethodPost).Path(storageRESTVersionPrefix + storageRESTMethodAppendFile).HandlerFunc(h(server.AppendFileHandler))
			subrouter.Methods(http.MethodPost).Path(storageRESTVersionPrefix + storageRESTMethodWriteAll).HandlerFunc(h(server.WriteAllHandler))

			subrouter.Methods(http.MethodPost).Path(storageRESTVersionPrefix + storageRESTMethodReadVersion).HandlerFunc(h(server.ReadVersionHandler))
			subrouter.Methods(http.MethodPost).Path(storageRESTVersionPrefix + storageRESTMethodReadXL).HandlerFunc(h(server.ReadXLHandler))
			subrouter.Methods(http.MethodPost).Path(storageRESTVersionPrefix + storageRESTMethodCreateFile).HandlerFunc(h(server.CreateFileHandler))
			subrouter.Methods(http.MethodPost).Path(storageRESTVersionPrefix + storageRESTMethodReadAll).HandlerFunc(h(server.ReadAllHandler))
			subrouter.Methods(http.MethodPost).Path(storageRESTVersionPrefix + storageRESTMethodReadFile).HandlerFunc(h(server.ReadFileHandler))
			subrouter.Methods(http.MethodPost).Path(storageRESTVersionPrefix + storageRESTMethodReadFileStream).HandlerFunc(h(server.ReadFileStreamHandler))
			subrouter.Methods(http.MethodPost).Path(storageRESTVersionPrefix + storageRESTMethodListDir).HandlerFunc(h(server.ListDirHandler))

			subrouter.Methods(http.MethodPost).Path(storageRESTVersionPrefix + storageRESTMethodDeleteVersions).HandlerFunc(h(server.DeleteVersionsHandler))
			subrouter.Methods(http.MethodPost).Path(storageRESTVersionPrefix + storageRESTMethodRenameFile).HandlerFunc(h(server.RenameFileHandler))
			subrouter.Methods(http.MethodPost).Path(storageRESTVersionPrefix + storageRESTMethodVerifyFile).HandlerFunc(h(server.VerifyFileHandler))
			subrouter.Methods(http.MethodPost).Path(storageRESTVersionPrefix + storageRESTMethodStatInfoFile).HandlerFunc(h(server.StatInfoFile))
			subrouter.Methods(http.MethodPost).Path(storageRESTVersionPrefix + storageRESTMethodReadMultiple).HandlerFunc(h(server.ReadMultiple))
			subrouter.Methods(http.MethodPost).Path(storageRESTVersionPrefix + storageRESTMethodCleanAbandoned).HandlerFunc(h(server.CleanAbandonedDataHandler))
			logger.FatalIf(storageRenameDataHandler.Register(gm, server.RenameDataHandler, endpoint.Path), "unable to register handler")
			logger.FatalIf(storageDeleteFileHandler.Register(gm, server.DeleteFileHandler, endpoint.Path), "unable to register handler")
			logger.FatalIf(storageCheckPartsHandler.Register(gm, server.CheckPartsHandler, endpoint.Path), "unable to register handler")
			logger.FatalIf(storageReadVersionHandler.Register(gm, server.ReadVersionHandlerWS, endpoint.Path), "unable to register handler")
			logger.FatalIf(storageWriteMetadataHandler.Register(gm, server.WriteMetadataHandler, endpoint.Path), "unable to register handler")
			logger.FatalIf(storageUpdateMetadataHandler.Register(gm, server.UpdateMetadataHandler, endpoint.Path), "unable to register handler")
			logger.FatalIf(storageDeleteVersionHandler.Register(gm, server.DeleteVersionHandler, endpoint.Path), "unable to register handler")
			logger.FatalIf(storageReadXLHandler.Register(gm, server.ReadXLHandlerWS, endpoint.Path), "unable to register handler")
			logger.FatalIf(storageNSScannerHandler.RegisterNoInput(gm, server.NSScannerHandler, endpoint.Path), "unable to register handler")
			logger.FatalIf(storageDiskInfoHandler.Register(gm, server.DiskInfoHandler, endpoint.Path), "unable to register handler")
			logger.FatalIf(storageStatVolHandler.Register(gm, server.StatVolHandler, endpoint.Path), "unable to register handler")
			logger.FatalIf(gm.RegisterStreamingHandler(grid.HandlerWalkDir, grid.StreamHandler{
				Subroute:    endpoint.Path,
				Handle:      server.WalkDirHandler,
				OutCapacity: 1,
			}), "unable to register handler")

			createStorage := func(server *storageRESTServer) bool {
				xl, err := newXLStorage(endpoint, false)
				if err != nil {
					// if supported errors don't fail, we proceed to
					// printing message and moving forward.
					logFatalErrs(err, endpoint, false)
					return false
				}
				storage := newXLStorageDiskIDCheck(xl, true)
				storage.SetDiskID(xl.diskID)

				globalLocalDrivesMu.Lock()
				defer globalLocalDrivesMu.Unlock()

				globalLocalDrives = append(globalLocalDrives, storage)
				globalLocalSetDrives[endpoint.PoolIdx][endpoint.SetIdx][endpoint.DiskIdx] = storage
				return true
			}

			if createStorage(server) {
				continue
			}

			// Start async goroutine to create storage.
			go func(server *storageRESTServer) {
				for {
					time.Sleep(3 * time.Second)
					if createStorage(server) {
						return
					}
				}
			}(server)

		}
	}
}
