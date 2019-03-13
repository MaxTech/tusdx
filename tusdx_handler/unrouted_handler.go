package tusdx_handler

import (
    "errors"
    "github.com/MaxTech/log"
    "github.com/MaxTech/tusdx"
    "github.com/gin-gonic/gin"
    "github.com/tus/tusd"
    "io"
    "math"
    "net"
    "net/http"
    "os"
    "regexp"
    "strconv"
    "sync/atomic"
    "time"
)

var (
    reExtractFileID  = regexp.MustCompile(`([^/]+)\/?$`)
    reForwardedHost  = regexp.MustCompile(`host=([^,]+)`)
    reForwardedProto = regexp.MustCompile(`proto=(https?)`)
    reMimeType       = regexp.MustCompile(`^[a-z]+\/[a-z\-\+]+$`)
)

type unroutedHandler struct {
    config        Config
    composer      *tusd.StoreComposer
    isBasePathAbs bool
    basePath      string
    logger        log.AppLogger
    extensions    string

    // CompleteUploads is used to send notifications whenever an upload is
    // completed by a user. The FileInfo will contain information about this
    // upload after it is completed. Sending to this channel will only
    // happen if the NotifyCompleteUploads field is set to true in the Config
    // structure. Notifications will also be sent for completions using the
    // Concatenation extension.
    CompleteUploads chan tusd.FileInfo
    // TerminatedUploads is used to send notifications whenever an upload is
    // terminated by a user. The FileInfo will contain information about this
    // upload gathered before the termination. Sending to this channel will only
    // happen if the NotifyTerminatedUploads field is set to true in the Config
    // structure.
    TerminatedUploads chan tusd.FileInfo
    // UploadProgress is used to send notifications about the progress of the
    // currently running uploads. For each open PATCH request, every second
    // a FileInfo instance will be send over this channel with the Offset field
    // being set to the number of bytes which have been transfered to the server.
    // Please be aware that this number may be higher than the number of bytes
    // which have been stored by the data store! Sending to this channel will only
    // happen if the NotifyUploadProgress field is set to true in the Config
    // structure.
    UploadProgress chan tusd.FileInfo
    // CreatedUploads is used to send notifications about the uploads having been
    // created. It triggers post creation and therefore has all the FileInfo incl.
    // the ID available already. It facilitates the post-create hook. Sending to
    // this channel will only happen if the NotifyCreatedUploads field is set to
    // true in the Config structure.
    CreatedUploads chan tusd.FileInfo
    // Metrics provides numbers of the usage for this handler.
    Metrics Metrics
}

// PostFile creates a new file upload using the datastore after validating the
// length and parsing the metadata.
func (handler *unroutedHandler) PostFile(context *gin.Context) {
    // Check for presence of application/offset+octet-stream. If another content
    // type is defined, it will be ignored and treated as none was set because
    // some HTTP clients may enforce a default value for this header.
    containsChunk := context.Request.Header.Get("Content-Type") == "application/offset+octet-stream"

    // Only use the proper Upload-Concat header if the concatenation extension
    // is even supported by the data store.
    var concatHeader string
    if handler.composer.UsesConcater {
        concatHeader = context.Request.Header.Get("Upload-Concat")
    }

    // Parse Upload-Concat header
    isPartial, isFinal, partialUploads, err := parseConcat(concatHeader)
    if err != nil {
        handler.sendError(context, err)
        return
    }

    // If the upload is a final upload created by concatenation multiple partial
    // uploads the size is sum of all sizes of these files (no need for
    // Upload-Length header)
    var size int64
    var sizeIsDeferred bool
    if isFinal {
        // A final upload must not contain a chunk within the creation request
        if containsChunk {
            handler.sendError(context, tusd.ErrModifyFinal)
            return
        }

        size, err = handler.sizeOfUploads(partialUploads)
        if err != nil {
            handler.sendError(context, err)
            return
        }
    } else {
        uploadLengthHeader := context.Request.Header.Get("Upload-Length")
        uploadDeferLengthHeader := context.Request.Header.Get("Upload-Defer-Length")
        size, sizeIsDeferred, err = handler.validateNewUploadLengthHeaders(uploadLengthHeader, uploadDeferLengthHeader)
        if err != nil {
            handler.sendError(context, err)
            return
        }
    }

    // Test whether the size is still allowed
    if handler.config.MaxSize > 0 && size > handler.config.MaxSize {
        handler.sendError(context, tusd.ErrMaxSizeExceeded)
        return
    }

    // Parse metadata
    meta := ParseMetadataHeader(context.Request.Header.Get("Upload-Metadata"))

    info := tusd.FileInfo{
        Size:           size,
        SizeIsDeferred: sizeIsDeferred,
        MetaData:       meta,
        IsPartial:      isPartial,
        IsFinal:        isFinal,
        PartialUploads: partialUploads,
    }

    id, err := handler.composer.Core.NewUpload(info)
    if err != nil {
        handler.sendError(context, err)
        return
    }

    info.ID = id

    // Add the Location header directly after creating the new resource to even
    // include it in cases of failure when an error is returned
    url := handler.absFileURL(context.Request, id)
    context.Writer.Header().Set("Location", url)

    go handler.Metrics.incUploadsCreated()
    handler.logger.Info("UploadCreated:", "\tid:", id, "\tsize:", i64toa(size), "\turl:", url)

    if handler.config.NotifyCreatedUploads {
        handler.CreatedUploads <- info
    }

    if isFinal {
        if err := handler.composer.Concater.ConcatUploads(id, partialUploads); err != nil {
            handler.sendError(context, err)
            return
        }
        info.Offset = size

        if handler.config.NotifyCompleteUploads {
            handler.CompleteUploads <- info
        }
    }

    if containsChunk {
        if handler.composer.UsesLocker {
            locker := handler.composer.Locker
            if err := locker.LockUpload(id); err != nil {
                handler.sendError(context, err)
                return
            }

            defer locker.UnlockUpload(id)
        }

        if err := handler.writeChunk(id, info, context); err != nil {
            handler.sendError(context, err)
            return
        }
    } else if !sizeIsDeferred && size == 0 {
        // Directly finish the upload if the upload is empty (i.e. has a size of 0).
        // This statement is in an else-if block to avoid causing duplicate calls
        // to finishUploadIfComplete if an upload is empty and contains a chunk.
        handler.finishUploadIfComplete(info)
    }

    context.Writer.Header().Set("Id", id)

    handler.sendResp(context, http.StatusCreated)
}

// PatchFile adds a chunk to an upload. This operation is only allowed
// if enough space in the upload is left.
func (handler *unroutedHandler) PatchFile(context *gin.Context) {

    // Check for presence of application/offset+octet-stream
    if context.Request.Header.Get("Content-Type") != "application/offset+octet-stream" {
        handler.sendError(context, tusd.ErrInvalidContentType)
        return
    }

    // Check for presence of a valid Upload-Offset Header
    offset, err := strconv.ParseInt(context.Request.Header.Get("Upload-Offset"), 10, 64)
    if err != nil || offset < 0 {
        handler.sendError(context, tusd.ErrInvalidOffset)
        return
    }

    id, err := extractIDFromPath(context.Request.URL.Path)
    if err != nil {
        handler.sendError(context, err)
        return
    }

    if handler.composer.UsesLocker {
        locker := handler.composer.Locker
        if err := locker.LockUpload(id); err != nil {
            handler.sendError(context, err)
            return
        }

        defer locker.UnlockUpload(id)
    }

    info, err := handler.composer.Core.GetInfo(id)
    if err != nil {
        handler.sendError(context, err)
        return
    }

    // Modifying a final upload is not allowed
    if info.IsFinal {
        handler.sendError(context, tusd.ErrModifyFinal)
        return
    }

    if offset != info.Offset {
        handler.sendError(context, tusd.ErrMismatchOffset)
        return
    }

    // Do not proxy the call to the data store if the upload is already completed
    if !info.SizeIsDeferred && info.Offset == info.Size {
        context.Writer.Header().Set("Upload-Offset", strconv.FormatInt(offset, 10))
        handler.sendResp(context, http.StatusNoContent)
        return
    }

    if context.Request.Header.Get("Upload-Length") != "" {
        if !handler.composer.UsesLengthDeferrer {
            handler.sendError(context, tusd.ErrNotImplemented)
            return
        }
        if !info.SizeIsDeferred {
            handler.sendError(context, tusd.ErrInvalidUploadLength)
            return
        }
        uploadLength, err := strconv.ParseInt(context.Request.Header.Get("Upload-Length"), 10, 64)
        if err != nil || uploadLength < 0 || uploadLength < info.Offset || (handler.config.MaxSize > 0 && uploadLength > handler.config.MaxSize) {
            handler.sendError(context, tusd.ErrInvalidUploadLength)
            return
        }
        if err := handler.composer.LengthDeferrer.DeclareLength(id, uploadLength); err != nil {
            handler.sendError(context, err)
            return
        }

        info.Size = uploadLength
        info.SizeIsDeferred = false
    }

    if err := handler.writeChunk(id, info, context); err != nil {
        handler.sendError(context, err)
        return
    }

    handler.sendResp(context, http.StatusNoContent)
}

// HeadFile returns the length and offset for the HEAD request
func (handler *unroutedHandler) HeadFile(context *gin.Context) {

    id, err := extractIDFromPath(context.Request.URL.Path)
    if err != nil {
        handler.sendError(context, err)
        return
    }

    if handler.composer.UsesLocker {
        locker := handler.composer.Locker
        if err := locker.LockUpload(id); err != nil {
            handler.sendError(context, err)
            return
        }

        defer locker.UnlockUpload(id)
    }

    info, err := handler.composer.Core.GetInfo(id)
    if err != nil {
        handler.sendError(context, err)
        return
    }

    // Add Upload-Concat header if possible
    if info.IsPartial {
        context.Writer.Header().Set("Upload-Concat", "partial")
    }

    if info.IsFinal {
        v := "final;"
        for _, uploadID := range info.PartialUploads {
            v += handler.absFileURL(context.Request, uploadID) + " "
        }
        // Remove trailing space
        v = v[:len(v)-1]

        context.Writer.Header().Set("Upload-Concat", v)
    }

    if len(info.MetaData) != 0 {
        context.Writer.Header().Set("Upload-Metadata", SerializeMetadataHeader(info.MetaData))
    }

    if info.SizeIsDeferred {
        context.Writer.Header().Set("Upload-Defer-Length", tusd.UploadLengthDeferred)
    } else {
        context.Writer.Header().Set("Upload-Length", strconv.FormatInt(info.Size, 10))
    }

    context.Writer.Header().Set("Cache-Control", "no-store")
    context.Writer.Header().Set("Upload-Offset", strconv.FormatInt(info.Offset, 10))
    handler.sendResp(context, http.StatusOK)
}

// GetFile handles requests to download a file using a GET request. This is not
// part of the specification.
func (handler *unroutedHandler) GetFile(context *gin.Context) {
    if !handler.composer.UsesGetReader {
        handler.sendError(context, tusd.ErrNotImplemented)
        return
    }

    id, err := extractIDFromPath(context.Request.URL.Path)
    if err != nil {
        handler.sendError(context, err)
        return
    }

    if len(id) < 4 {
        handler.sendError(context, errors.New("id 长度错误"))
        return
    }

    if handler.composer.UsesLocker {
        locker := handler.composer.Locker

        if err := locker.LockUpload(id); err != nil {
            handler.sendError(context, err)
            return
        }

        defer locker.UnlockUpload(id)
    }

    info, err := handler.composer.Core.GetInfo(id)
    if err != nil {
        handler.sendError(context, err)
        return
    }

    // Set headers before sending responses
    context.Writer.Header().Set("Content-Length", strconv.FormatInt(info.Offset, 10))

    contentType, contentDisposition := filterContentType(info)
    context.Writer.Header().Set("Content-Type", contentType)
    context.Writer.Header().Set("Content-Disposition", contentDisposition)

    // If no data has been uploaded yet, respond with an empty "204 No Content" status.
    if info.Offset == 0 {
        handler.sendResp(context, http.StatusNoContent)
        return
    }

    src, err := handler.composer.GetReader.GetReader(id)
    if err != nil {
        handler.sendError(context, err)
        return
    }

    handler.sendResp(context, http.StatusOK)
    io.Copy(context.Writer, src)

    // Try to close the reader if the io.Closer interface is implemented
    if closer, ok := src.(io.Closer); ok {
        closer.Close()
    }
}

// DelFile terminates an upload permanently.
func (handler *unroutedHandler) DelFile(context *gin.Context) {
    // Abort the request handling if the required interface is not implemented
    if !handler.composer.UsesTerminater {
        handler.sendError(context, tusd.ErrNotImplemented)
        return
    }

    id, err := extractIDFromPath(context.Request.URL.Path)
    if err != nil {
        handler.sendError(context, err)
        return
    }

    if handler.composer.UsesLocker {
        locker := handler.composer.Locker
        if err := locker.LockUpload(id); err != nil {
            handler.sendError(context, err)
            return
        }

        defer locker.UnlockUpload(id)
    }

    var info tusd.FileInfo
    if handler.config.NotifyTerminatedUploads {
        info, err = handler.composer.Core.GetInfo(id)
        if err != nil {
            handler.sendError(context, err)
            return
        }
    }

    err = handler.composer.Terminater.Terminate(id)
    if err != nil {
        handler.sendError(context, err)
        return
    }

    handler.sendResp(context, http.StatusNoContent)

    if handler.config.NotifyTerminatedUploads {
        handler.TerminatedUploads <- info
    }

    go handler.Metrics.incUploadsTerminated()
}

// The get sum of all sizes for a list of upload ids while checking whether
// all of these uploads are finished yet. This is used to calculate the size
// of a final resource.
func (handler *unroutedHandler) sizeOfUploads(ids []string) (size int64, err error) {
    for _, id := range ids {
        info, err := tusdx.Composer.Core.GetInfo(id)
        if err != nil {
            return size, err
        }

        if info.SizeIsDeferred || info.Offset != info.Size {
            err = tusd.ErrUploadNotFinished
            return size, err
        }

        size += info.Size
    }

    return
}

// Verify that the Upload-Length and Upload-Defer-Length headers are acceptable for creating a
// new upload
func (handler *unroutedHandler) validateNewUploadLengthHeaders(uploadLengthHeader string, uploadDeferLengthHeader string) (uploadLength int64, uploadLengthDeferred bool, err error) {
    haveBothLengthHeaders := uploadLengthHeader != "" && uploadDeferLengthHeader != ""
    haveInvalidDeferHeader := uploadDeferLengthHeader != "" && uploadDeferLengthHeader != tusd.UploadLengthDeferred
    lengthIsDeferred := uploadDeferLengthHeader == tusd.UploadLengthDeferred

    if lengthIsDeferred && !tusdx.Composer.UsesLengthDeferrer {
        err = tusd.ErrNotImplemented
    } else if haveBothLengthHeaders {
        err = tusd.ErrUploadLengthAndUploadDeferLength
    } else if haveInvalidDeferHeader {
        err = tusd.ErrInvalidUploadDeferLength
    } else if lengthIsDeferred {
        uploadLengthDeferred = true
    } else {
        uploadLength, err = strconv.ParseInt(uploadLengthHeader, 10, 64)
        if err != nil || uploadLength < 0 {
            err = tusd.ErrInvalidUploadLength
        }
    }

    return
}

func (handler *unroutedHandler) sendError(context *gin.Context, err error) {
    // Interpret os.ErrNotExist as 404 Not Found
    if os.IsNotExist(err) {
        err = tusd.ErrNotFound
    }

    // Errors for read timeouts contain too much information which is not
    // necessary for us and makes grouping for the metrics harder. The error
    // message looks like: read tcp 127.0.0.1:1080->127.0.0.1:53673: i/o timeout
    // Therefore, we use a common error message for all of them.
    if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
        err = errors.New("read tcp: i/o timeout")
    }

    statusErr, ok := err.(tusd.HTTPError)
    if !ok {
        statusErr = tusd.NewHTTPError(err, http.StatusInternalServerError)
    }

    reason := err.Error() + "\n"
    if context.Request.Method == "HEAD" {
        reason = ""
    }

    context.Writer.WriteHeader(statusErr.StatusCode())

    context.Set("tusd-error", reason)

    handler.logger.Info("ResponseOutgoing", "\tstatus:", strconv.Itoa(statusErr.StatusCode()), "\tmethod:", context.Request.Method, "\tpath:", context.Request.URL.Path, "\terror:", err.Error())

    return
}

// sendResp writes the header to w with the specified status code.
func (handler *unroutedHandler) sendResp(context *gin.Context, status int) {
    context.Writer.WriteHeader(status)

    handler.logger.Info("ResponseOutgoing", "\tstatus:", strconv.Itoa(status), "\tmethod:", context.Request.Method, "\tpath:", context.Request.URL.Path)

    return
}

// Make an absolute URLs to the given upload id. If the base path is absolute
// it will be prepended else the host and protocol from the request is used.
func (handler *unroutedHandler) absFileURL(r *http.Request, id string) string {
    if handler.isBasePathAbs {
        return handler.basePath + id
    }

    // Read origin and protocol from request
    host, proto := getHostAndProtocol(r, handler.config.RespectForwardedHeaders)

    url := proto + "://" + host + handler.basePath + id

    return url
}

// writeChunk reads the body from the requests r and appends it to the upload
// with the corresponding id. Afterwards, it will set the necessary response
// headers but will not send the response.
func (handler *unroutedHandler) writeChunk(id string, info tusd.FileInfo, context *gin.Context) error {
    // Get Content-Length if possible
    length := context.Request.ContentLength
    offset := info.Offset

    // Test if this upload fits into the file's size
    if !info.SizeIsDeferred && offset+length > info.Size {
        return tusd.ErrSizeExceeded
    }

    maxSize := info.Size - offset
    // If the upload's length is deferred and the PATCH request does not contain the Content-Length
    // header (which is allowed if 'Transfer-Encoding: chunked' is used), we still need to set limits for
    // the body size.
    if info.SizeIsDeferred {
        if handler.config.MaxSize > 0 {
            // Ensure that the upload does not exceed the maximum upload size
            maxSize = handler.config.MaxSize - offset
        } else {
            // If no upload limit is given, we allow arbitrary sizes
            maxSize = math.MaxInt64
        }
    }
    if length > 0 {
        maxSize = length
    }

    handler.logger.Info("ChunkWriteStart", "\tid:", id, "\tmaxSize:", i64toa(maxSize), "\toffset:", i64toa(offset))

    var bytesWritten int64
    // Prevent a nil pointer dereference when accessing the body which may not be
    // available in the case of a malicious request.
    if context.Request.Body != nil {
        // Limit the data read from the request's body to the allowed maximum
        reader := io.LimitReader(context.Request.Body, maxSize)

        if handler.config.NotifyUploadProgress {
            var stop chan<- struct{}
            reader, stop = handler.sendProgressMessages(info, reader)
            defer close(stop)
        }

        var err error
        bytesWritten, err = handler.composer.Core.WriteChunk(id, offset, reader)
        if err != nil {
            return err
        }
    }

    handler.logger.Info("ChunkWriteComplete", "\tid:", id, "\tbytesWritten:", i64toa(bytesWritten))

    // Send new offset to client
    newOffset := offset + bytesWritten
    context.Writer.Header().Set("Upload-Offset", strconv.FormatInt(newOffset, 10))
    go handler.Metrics.incBytesReceived(uint64(bytesWritten))
    info.Offset = newOffset

    return handler.finishUploadIfComplete(info)
}

// finishUploadIfComplete checks whether an upload is completed (i.e. upload offset
// matches upload size) and if so, it will call the data store's FinishUpload
// function and send the necessary message on the CompleteUpload channel.
func (handler *unroutedHandler) finishUploadIfComplete(info tusd.FileInfo) error {
    // If the upload is completed, ...
    if !info.SizeIsDeferred && info.Offset == info.Size {
        // ... allow custom mechanism to finish and cleanup the upload
        if handler.composer.UsesFinisher {
            if err := handler.composer.Finisher.FinishUpload(info.ID); err != nil {
                return err
            }
        }

        // ... send the info out to the channel
        if handler.config.NotifyCompleteUploads {
            handler.CompleteUploads <- info
        }

        go handler.Metrics.incUploadsFinished()
    }

    return nil
}

// sendProgressMessage will send a notification over the UploadProgress channel
// every second, indicating how much data has been transfered to the server.
// It will stop sending these instances once the returned channel has been
// closed. The returned reader should be used to read the request body.
func (handler *unroutedHandler) sendProgressMessages(info tusd.FileInfo, reader io.Reader) (io.Reader, chan<- struct{}) {
    progress := &progressWriter{
        Offset: info.Offset,
    }
    stop := make(chan struct{}, 1)
    reader = io.TeeReader(reader, progress)

    go func() {
        for {
            select {
            case <-stop:
                info.Offset = atomic.LoadInt64(&progress.Offset)
                handler.UploadProgress <- info
                return
            case <-time.After(1 * time.Second):
                info.Offset = atomic.LoadInt64(&progress.Offset)
                handler.UploadProgress <- info
            }
        }
    }()

    return reader, stop
}

func (handler *unroutedHandler) OptionFile(context *gin.Context) {
    if handler.config.MaxSize > 0 {
        context.Request.Header.Set("Tus-Max-Size", strconv.FormatInt(handler.config.MaxSize, 10))
    }

    context.Request.Header.Set("Tus-Version", "1.0.0")
    context.Request.Header.Set("Tus-Extension", handler.extensions)

    // Although the 204 No Content status code is a better fit in this case,
    // since we do not have a response body included, we cannot use it here
    // as some browsers only accept 200 OK as successful response to a
    // preflight request. If we send them the 204 No Content the response
    // will be ignored or interpreted as a rejection.
    // For example, the Presto engine, which is used in older versions of
    // Opera, Opera Mobile and Opera Mini, handles CORS this way.
    handler.sendResp(context, http.StatusOK)
    return
}
