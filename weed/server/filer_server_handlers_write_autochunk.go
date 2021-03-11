package weed_server

import (
	"archive/zip"
	"context"
	"crypto/md5"
	"fmt"
	"hash"
	"io"
	"io/ioutil"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chrislusf/seaweedfs/weed/filer"
	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/operation"
	"github.com/chrislusf/seaweedfs/weed/pb/filer_pb"
	xhttp "github.com/chrislusf/seaweedfs/weed/s3api/http"
	"github.com/chrislusf/seaweedfs/weed/security"
	"github.com/chrislusf/seaweedfs/weed/stats"
	"github.com/chrislusf/seaweedfs/weed/storage/needle"
	"github.com/chrislusf/seaweedfs/weed/util"
)

func (fs *FilerServer) autoChunk(ctx context.Context, w http.ResponseWriter, r *http.Request, so *operation.StorageOption) {

	// autoChunking can be set at the command-line level or as a query param. Query param overrides command-line
	query := r.URL.Query()

	parsedMaxMB, _ := strconv.ParseInt(query.Get("maxMB"), 10, 32)
	maxMB := int32(parsedMaxMB)
	if maxMB <= 0 && fs.option.MaxMB > 0 {
		maxMB = int32(fs.option.MaxMB)
	}

	chunkSize := 1024 * 1024 * maxMB

	stats.FilerRequestCounter.WithLabelValues("postAutoChunk").Inc()
	start := time.Now()
	defer func() {
		stats.FilerRequestHistogram.WithLabelValues("postAutoChunk").Observe(time.Since(start).Seconds())
	}()

	var reply = make([]*FilerPostResult, 1)
	var err error
	var md5bytes []byte
	if r.Method == "POST" {
		if r.Header.Get("Content-Type") == "" && strings.HasSuffix(r.URL.Path, "/") {
			reply[0], err = fs.mkdir(ctx, w, r)

		} else {
			reply, md5bytes, err = fs.doPostAutoChunk(ctx, w, r, chunkSize, so)
		}
	} else {
		reply[0], md5bytes, err = fs.doPutAutoChunk(ctx, w, r, chunkSize, so)
	}

	if err != nil {
		if strings.HasPrefix(err.Error(), "read input:") {
			writeJsonError(w, r, 499, err)
		} else {
			writeJsonError(w, r, http.StatusInternalServerError, err)
		}
	} else if reply != nil {

		if len(md5bytes) > 0 {
			w.Header().Set("Content-MD5", util.Base64Encode(md5bytes))
		}

		if len(reply) == 1 {
			writeJsonQuiet(w, r, http.StatusCreated, reply[0])
		}else {
			writeJsonQuiet(w, r, http.StatusCreated, reply)
		}

	}
}

func (fs *FilerServer) doPostAutoChunk(ctx context.Context, w http.ResponseWriter, r *http.Request, chunkSize int32, so *operation.StorageOption) (filerResults []*FilerPostResult, md5bytes []byte, replyerr error) {

	multipartReader, multipartReaderErr := r.MultipartReader()
	if multipartReaderErr != nil {
		return nil, nil, multipartReaderErr
	}

	part1, part1Err := multipartReader.NextPart()
	if part1Err != nil {
		return nil, nil, part1Err
	}

	fileName := part1.FileName()
	if fileName != "" {
		fileName = path.Base(fileName)
	}
	contentType := part1.Header.Get("Content-Type")
	if contentType == "application/octet-stream" {
		contentType = ""
	}

	if strings.HasSuffix(fileName, ZipSuffix[1:]) {
		//说明是自定义的压缩文件
		filerResults, replyerr = fs.doZipPostAutoChunk(ctx, w, r, chunkSize, so, fileName, part1)

		if replyerr != nil {
			return nil, nil, replyerr
		}

	} else {

		filerResult, _md5bytes, err := fs.uploadReader(ctx, w, r, part1, chunkSize, fileName, "", so, r.URL.Query().Get("isVideo") == "true")

		if err != nil {
			return nil, nil, err
		}

		filerResults = append(filerResults,filerResult)
		md5bytes = _md5bytes

	}

	return
}

func (fs *FilerServer) doZipPostAutoChunk(ctx context.Context, w http.ResponseWriter, r *http.Request, chunkSize int32, so *operation.StorageOption, fileName string, part1 *multipart.Part) ([]*FilerPostResult, error) {

	tempFile, _ := ioutil.TempFile("", "filertemp_*")
	uploadBytes, _ := ioutil.ReadAll(io.TeeReader(part1, md5.New()))

	replyerr := ioutil.WriteFile(tempFile.Name(), uploadBytes, os.ModeTemporary)

	if replyerr != nil {
		return nil, replyerr
	}

	defer os.Remove(tempFile.Name())

	zipReader, replyerr := zip.OpenReader(tempFile.Name())

	if replyerr != nil {
		return nil, replyerr
	}

	defer zipReader.Close()
	filerResults := make( []*FilerPostResult, len(zipReader.File))

	for index, file := range zipReader.File {

		readCloser, err := file.Open()

		if err != nil {
			return nil, err
		}

		filerResult, _, replyerr := fs.uploadReader(ctx, w, r, readCloser, chunkSize, file.Name, "", so, false)

		if replyerr != nil {
			return nil, replyerr
		}
		filerResults[index] = filerResult

		_ = readCloser.Close()

	}
	return filerResults, nil
}

func (fs *FilerServer) doPutAutoChunk(ctx context.Context, w http.ResponseWriter, r *http.Request, chunkSize int32, so *operation.StorageOption) (filerResult *FilerPostResult, md5bytes []byte, replyerr error) {

	fileName := path.Base(r.URL.Path)
	contentType := r.Header.Get("Content-Type")
	if contentType == "application/octet-stream" {
		contentType = ""
	}

	filerResult, md5bytes, replyerr = fs.uploadReader(ctx, w, r, r.Body, chunkSize, fileName, contentType, so, false)

	return
}

func (fs *FilerServer) uploadReader(ctx context.Context, w http.ResponseWriter, r *http.Request, reader io.Reader, chunkSize int32, fileName string, contentType string, so *operation.StorageOption, isVideo bool) (filerResult *FilerPostResult, md5bytes []byte, err error) {

	var tempfilepath string

	if isVideo {

		bs, _ := ioutil.ReadAll(reader)
		file, _ := ioutil.TempFile("", "video_temp.*")
		file.Write(bs)
		file.Close()
		tempfilepath = file.Name()
		defer os.Remove(tempfilepath)

		reader = util.NewBytesReader(bs)

	}

	if contentType == "" {

		contentType = mime.TypeByExtension(strings.ToLower(filepath.Ext(fileName)))
	}

	if strings.HasPrefix(fileName, "/") {
		fileName = fileName[1:]
	}

	fileChunks, md5Hash, chunkOffset, err, smallContent := fs.uploadReaderToChunks(w, r, reader, chunkSize, fileName, contentType, so)
	if err != nil {
		return nil, nil, err
	}

	md5bytes = md5Hash.Sum(nil)
	filerResult, err = fs.saveMetaData(ctx, r, fileName, contentType, so, md5bytes, fileChunks, chunkOffset, smallContent)

	if err != nil {
		return nil, nil, err
	}

	if isVideo {

		//执行shell脚本
		picid := getUUID() + ".jpg"

		abs, _ := filepath.Abs(tempfilepath)
		dir := filepath.Dir(abs)

		temppicpath := dir + "/" + picid

		command := exec.Command(GetCurrentDirectory()+"shoot_cut.sh", tempfilepath, temppicpath)
		// cmd := fmt.Sprintf("-y -i %s -y -f image2 -ss 00:00:01 -t 0.001 %s", tempfilepath, temppicpath)
		// command := exec.Command("/usr/local/ffmpeg/bin/ffmpeg", strings.Split(cmd, " ")...)

		errPipe, _ := command.StderrPipe()
		stdout, _ := command.StdoutPipe()

		defer errPipe.Close()

		err := command.Start()

		if err != nil {

			return nil, nil, err
		}

		err = command.Wait()

		if err != nil {

			out_bytes, _ := ioutil.ReadAll(stdout)
			err_bytes, _ := ioutil.ReadAll(errPipe)

			return nil, nil, fmt.Errorf(string(out_bytes) + ": " + string(err_bytes) +": " + err.Error())
		}

		picBytes, err := ioutil.ReadFile(temppicpath)

		if err != nil {
			return nil, nil, err
		}

		defer os.Remove(temppicpath)

		path := r.URL.Path

		//处理指定文件上传时，cover覆盖video的bug
		if !strings.HasSuffix(path, "/") {
			path = filepath.Dir(path)
			r.URL.Path = path
		}

		_, _, err = fs.uploadReader(ctx, w, r, util.NewBytesReader(picBytes), chunkSize, picid, "", so, false)

		if err != nil {
			return nil, nil, err
		}
		filerResult.Cover = picid
	}



	return filerResult, md5bytes, err
}


func isAppend(r *http.Request) bool {
	return r.URL.Query().Get("op") == "append"
}

func (fs *FilerServer) saveMetaData(ctx context.Context, r *http.Request, fileName string, contentType string, so *operation.StorageOption, md5bytes []byte, fileChunks []*filer_pb.FileChunk, chunkOffset int64, content []byte) (filerResult *FilerPostResult, replyerr error) {

	// detect file mode
	modeStr := r.URL.Query().Get("mode")
	if modeStr == "" {
		modeStr = "0660"
	}
	mode, err := strconv.ParseUint(modeStr, 8, 32)
	if err != nil {
		glog.Errorf("Invalid mode format: %s, use 0660 by default", modeStr)
		mode = 0660
	}

	// fix the path
	path := r.URL.Path
	if strings.HasSuffix(path, "/") {
		if fileName != "" {
			path += fileName
		}
	}

	var entry *filer.Entry
	var mergedChunks []*filer_pb.FileChunk
	// when it is an append
	if isAppend(r) {
		existingEntry, findErr := fs.filer.FindEntry(ctx, util.FullPath(path))
		if findErr != nil && findErr != filer_pb.ErrNotFound {
			glog.V(0).Infof("failing to find %s: %v", path, findErr)
		}
		entry = existingEntry
	}
	if entry != nil {
		entry.Mtime = time.Now()
		entry.Md5 = nil
		// adjust chunk offsets
		for _, chunk := range fileChunks {
			chunk.Offset += int64(entry.FileSize)
		}
		mergedChunks = append(entry.Chunks, fileChunks...)
		entry.FileSize += uint64(chunkOffset)

		// TODO
		if len(entry.Content) > 0 {
			replyerr = fmt.Errorf("append to small file is not supported yet")
			return
		}

	} else {
		glog.V(4).Infoln("saving", path)
		mergedChunks = fileChunks
		entry = &filer.Entry{
			FullPath: util.FullPath(path),
			Attr: filer.Attr{
				Mtime:       time.Now(),
				Crtime:      time.Now(),
				Mode:        os.FileMode(mode),
				Uid:         OS_UID,
				Gid:         OS_GID,
				Replication: so.Replication,
				Collection:  so.Collection,
				TtlSec:      so.TtlSeconds,
				DiskType:    so.DiskType,
				Mime:        contentType,
				Md5:         md5bytes,
				FileSize:    uint64(chunkOffset),
			},
			Content: content,
		}
	}

	// maybe compact entry chunks
	mergedChunks, replyerr = filer.MaybeManifestize(fs.saveAsChunk(so), mergedChunks)
	if replyerr != nil {
		glog.V(0).Infof("manifestize %s: %v", r.RequestURI, replyerr)
		return
	}
	entry.Chunks = mergedChunks

	filerResult = &FilerPostResult{
		Name: fileName,
		Size: int64(entry.FileSize),
	}

	if entry.Extended == nil {
		entry.Extended = make(map[string][]byte)
	}

	fs.saveAmzMetaData(r, entry)

	for k, v := range r.Header {
		if len(v) > 0 && strings.HasPrefix(k, needle.PairNamePrefix) {
			entry.Extended[k] = []byte(v[0])
		}
	}

	if dbErr := fs.filer.CreateEntry(ctx, entry, false, false, nil); dbErr != nil {
		fs.filer.DeleteChunks(fileChunks)
		replyerr = dbErr
		filerResult.Error = dbErr.Error()
		glog.V(0).Infof("failing to write %s to filer server : %v", path, dbErr)
	}
	return filerResult, replyerr
}

func (fs *FilerServer) uploadReaderToChunks(w http.ResponseWriter, r *http.Request, reader io.Reader, chunkSize int32, fileName string, contentType string, so *operation.StorageOption) ([]*filer_pb.FileChunk, hash.Hash, int64, error, []byte) {
	var fileChunks []*filer_pb.FileChunk

	md5Hash := md5.New()
	var partReader = ioutil.NopCloser(io.TeeReader(reader, md5Hash))

	chunkOffset := int64(0)
	var smallContent []byte

	for {
		limitedReader := io.LimitReader(partReader, int64(chunkSize))

		data, err := ioutil.ReadAll(limitedReader)
		if err != nil {
			return nil, nil, 0, err, nil
		}
		if chunkOffset == 0 && !isAppend(r) {
			if len(data) < fs.option.SaveToFilerLimit || strings.HasPrefix(r.URL.Path, filer.DirectoryEtcRoot) && len(data) < 4*1024 {
				smallContent = data
				chunkOffset += int64(len(data))
				break
			}
		}
		dataReader := util.NewBytesReader(data)

		// retry to assign a different file id
		var fileId, urlLocation string
		var auth security.EncodedJwt
		var assignErr, uploadErr error
		var uploadResult *operation.UploadResult
		for i := 0; i < 3; i++ {
			// assign one file id for one chunk
			fileId, urlLocation, auth, assignErr = fs.assignNewFileInfo(so)
			if assignErr != nil {
				return nil, nil, 0, assignErr, nil
			}

			// upload the chunk to the volume server
			uploadResult, uploadErr, _ = fs.doUpload(urlLocation, w, r, dataReader, fileName, contentType, nil, auth)
			if uploadErr != nil {
				time.Sleep(251 * time.Millisecond)
				continue
			}
			break
		}
		if uploadErr != nil {
			return nil, nil, 0, uploadErr, nil
		}

		// if last chunk exhausted the reader exactly at the border
		if uploadResult.Size == 0 {
			break
		}

		// Save to chunk manifest structure
		fileChunks = append(fileChunks, uploadResult.ToPbFileChunk(fileId, chunkOffset))

		glog.V(4).Infof("uploaded %s chunk %d to %s [%d,%d)", fileName, len(fileChunks), fileId, chunkOffset, chunkOffset+int64(uploadResult.Size))

		// reset variables for the next chunk
		chunkOffset = chunkOffset + int64(uploadResult.Size)

		// if last chunk was not at full chunk size, but already exhausted the reader
		if int64(uploadResult.Size) < int64(chunkSize) {
			break
		}
	}

	return fileChunks, md5Hash, chunkOffset, nil, smallContent
}

func (fs *FilerServer) doUpload(urlLocation string, w http.ResponseWriter, r *http.Request, limitedReader io.Reader, fileName string, contentType string, pairMap map[string]string, auth security.EncodedJwt) (*operation.UploadResult, error, []byte) {

	stats.FilerRequestCounter.WithLabelValues("postAutoChunkUpload").Inc()
	start := time.Now()
	defer func() {
		stats.FilerRequestHistogram.WithLabelValues("postAutoChunkUpload").Observe(time.Since(start).Seconds())
	}()

	uploadResult, err, data := operation.Upload(urlLocation, fileName, fs.option.Cipher, limitedReader, false, contentType, pairMap, auth)
	return uploadResult, err, data
}

func (fs *FilerServer) saveAsChunk(so *operation.StorageOption) filer.SaveDataAsChunkFunctionType {

	return func(reader io.Reader, name string, offset int64) (*filer_pb.FileChunk, string, string, error) {
		// assign one file id for one chunk
		fileId, urlLocation, auth, assignErr := fs.assignNewFileInfo(so)
		if assignErr != nil {
			return nil, "", "", assignErr
		}

		// upload the chunk to the volume server
		uploadResult, uploadErr, _ := operation.Upload(urlLocation, name, fs.option.Cipher, reader, false, "", nil, auth)
		if uploadErr != nil {
			return nil, "", "", uploadErr
		}

		return uploadResult.ToPbFileChunk(fileId, offset), so.Collection, so.Replication, nil
	}
}

func (fs *FilerServer) mkdir(ctx context.Context, w http.ResponseWriter, r *http.Request) (filerResult *FilerPostResult, replyerr error) {

	// detect file mode
	modeStr := r.URL.Query().Get("mode")
	if modeStr == "" {
		modeStr = "0660"
	}
	mode, err := strconv.ParseUint(modeStr, 8, 32)
	if err != nil {
		glog.Errorf("Invalid mode format: %s, use 0660 by default", modeStr)
		mode = 0660
	}

	// fix the path
	path := r.URL.Path
	if strings.HasSuffix(path, "/") {
		path = path[:len(path)-1]
	}

	existingEntry, err := fs.filer.FindEntry(ctx, util.FullPath(path))
	if err == nil && existingEntry != nil {
		replyerr = fmt.Errorf("dir %s already exists", path)
		return
	}

	glog.V(4).Infoln("mkdir", path)
	entry := &filer.Entry{
		FullPath: util.FullPath(path),
		Attr: filer.Attr{
			Mtime:  time.Now(),
			Crtime: time.Now(),
			Mode:   os.FileMode(mode) | os.ModeDir,
			Uid:    OS_UID,
			Gid:    OS_GID,
		},
	}

	filerResult = &FilerPostResult{
		Name: util.FullPath(path).Name(),
	}

	if dbErr := fs.filer.CreateEntry(ctx, entry, false, false, nil); dbErr != nil {
		replyerr = dbErr
		filerResult.Error = dbErr.Error()
		glog.V(0).Infof("failing to create dir %s on filer server : %v", path, dbErr)
	}
	return filerResult, replyerr
}

func (fs *FilerServer) saveAmzMetaData(r *http.Request, entry *filer.Entry) {

	if sc := r.Header.Get(xhttp.AmzStorageClass); sc != "" {
		entry.Extended[xhttp.AmzStorageClass] = []byte(sc)
	}

	if tags := r.Header.Get(xhttp.AmzObjectTagging); tags != "" {
		for _, v := range strings.Split(tags, "&") {
			tag := strings.Split(v, "=")
			if len(tag) == 2 {
				entry.Extended[xhttp.AmzObjectTagging+"-"+tag[0]] = []byte(tag[1])
			}
		}
	}

	for header, values := range r.Header {
		if strings.HasPrefix(header, xhttp.AmzUserMetaPrefix) {
			for _, value := range values {
				entry.Extended[header] = []byte(value)
			}
		}
	}
}
