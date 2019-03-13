package tusdx_handler

import (
    "encoding/base64"
    "github.com/tus/tusd"
    "net/http"
    "strconv"
    "strings"
)

var handler *unroutedHandler = nil

// mimeInlineBrowserWhitelist is a map containing MIME types which should be
// allowed to be rendered by browser inline, instead of being forced to be
// downloadd. For example, HTML or SVG files are not allowed, since they may
// contain malicious JavaScript. In a similiar fashion PDF is not on this list
// as their parsers commonly contain vulnerabilities which can be exploited.
// The values of this map does not convei any meaning and are therefore just
// empty structs.
var mimeInlineBrowserWhitelist = map[string]struct{}{
    "text/plain": struct{}{},

    "image/png":  struct{}{},
    "image/jpeg": struct{}{},
    "image/gif":  struct{}{},
    "image/bmp":  struct{}{},
    "image/webp": struct{}{},

    "audio/wave":      struct{}{},
    "audio/wav":       struct{}{},
    "audio/x-wav":     struct{}{},
    "audio/x-pn-wav":  struct{}{},
    "audio/webm":      struct{}{},
    "video/webm":      struct{}{},
    "audio/ogg":       struct{}{},
    "video/ogg ":      struct{}{},
    "application/ogg": struct{}{},
}

// filterContentType returns the values for the Content-Type and
// Content-Disposition headers for a given upload. These values should be used
// in responses for GET requests to ensure that only non-malicious file types
// are shown directly in the browser. It will extract the file name and type
// from the "fileame" and "filetype".
// See https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Content-Disposition
func filterContentType(info tusd.FileInfo) (contentType string, contentDisposition string) {
    filetype := info.MetaData["filetype"]

    if reMimeType.MatchString(filetype) {
        // If the filetype from metadata is well formed, we forward use this
        // for the Content-Type header. However, only whitelisted mime types
        // will be allowed to be shown inline in the browser
        contentType = filetype
        if _, isWhitelisted := mimeInlineBrowserWhitelist[filetype]; isWhitelisted {
            contentDisposition = "inline"
        } else {
            contentDisposition = "attachment"
        }
    } else {
        // If the filetype from the metadata is not well formed, we use a
        // default type and force the browser to download the content.
        contentType = "application/octet-stream"
        contentDisposition = "attachment"
    }

    // Add a filename to Content-Disposition if one is available in the metadata
    if filename, ok := info.MetaData["filename"]; ok {
        contentDisposition += ";filename=" + strconv.Quote(filename)
    }

    return contentType, contentDisposition
}

// Parse the Upload-Concat header, e.g.
// Upload-Concat: partial
// Upload-Concat: final;http://tus.io/files/a /files/b/
func parseConcat(header string) (isPartial bool, isFinal bool, partialUploads []string, err error) {
    if len(header) == 0 {
        return
    }

    if header == "partial" {
        isPartial = true
        return
    }

    l := len("final;")
    if strings.HasPrefix(header, "final;") && len(header) > l {
        isFinal = true

        list := strings.Split(header[l:], " ")
        for _, value := range list {
            value := strings.TrimSpace(value)
            if value == "" {
                continue
            }

            id, extractErr := extractIDFromPath(value)
            if extractErr != nil {
                err = extractErr
                return
            }

            partialUploads = append(partialUploads, id)
        }
    }

    // If no valid partial upload ids are extracted this is not a final upload.
    if len(partialUploads) == 0 {
        isFinal = false
        err = tusd.ErrInvalidConcat
    }

    return
}

// extractIDFromPath pulls the last segment from the url provided
func extractIDFromPath(url string) (string, error) {
    result := reExtractFileID.FindStringSubmatch(url)
    if len(result) != 2 {
        return "", tusd.ErrNotFound
    }
    return result[1], nil
}

func i64toa(num int64) string {
    return strconv.FormatInt(num, 10)
}

// ParseMetadataHeader parses the Upload-Metadata header as defined in the
// File Creation extension.
// e.g. Upload-Metadata: name bHVucmpzLnBuZw==,type aW1hZ2UvcG5n
func ParseMetadataHeader(header string) map[string]string {
    meta := make(map[string]string)

    for _, element := range strings.Split(header, ",") {
        element := strings.TrimSpace(element)

        parts := strings.Split(element, " ")

        // Do not continue with this element if no key and value or presented
        if len(parts) != 2 {
            continue
        }

        // Ignore corrent element if the value is no valid base64
        key := parts[0]
        value, err := base64.StdEncoding.DecodeString(parts[1])
        if err != nil {
            continue
        }

        meta[key] = string(value)
    }

    return meta
}

// SerializeMetadataHeader serializes a map of strings into the Upload-Metadata
// header format used in the response for HEAD requests.
// e.g. Upload-Metadata: name bHVucmpzLnBuZw==,type aW1hZ2UvcG5n
func SerializeMetadataHeader(meta map[string]string) string {
    header := ""
    for key, value := range meta {
        valueBase64 := base64.StdEncoding.EncodeToString([]byte(value))
        header += key + " " + valueBase64 + ","
    }

    // Remove trailing comma
    if len(header) > 0 {
        header = header[:len(header)-1]
    }

    return header
}

// getHostAndProtocol extracts the host and used protocol (either HTTP or HTTPS)
// from the given request. If `allowForwarded` is set, the X-Forwarded-Host,
// X-Forwarded-Proto and Forwarded headers will also be checked to
// support proxies.
func getHostAndProtocol(r *http.Request, allowForwarded bool) (host, proto string) {
    if r.TLS != nil {
        proto = "https"
    } else {
        proto = "http"
    }

    host = r.Host

    if !allowForwarded {
        return
    }

    if h := r.Header.Get("X-Forwarded-Host"); h != "" {
        host = h
    }

    if h := r.Header.Get("X-Forwarded-Proto"); h == "http" || h == "https" {
        proto = h
    }

    if h := r.Header.Get("Forwarded"); h != "" {
        if r := reForwardedHost.FindStringSubmatch(h); len(r) == 2 {
            host = r[1]
        }

        if r := reForwardedProto.FindStringSubmatch(h); len(r) == 2 {
            proto = r[1]
        }
    }

    return
}

// NewUnroutedHandler creates a new handler without routing using the given
// configuration. It exposes the http handlers which need to be combined with
// a router (aka mux) of your choice. If you are looking for preconfigured
// handler see NewHandler.
func NewUnroutedHandler(config Config) (*unroutedHandler, error) {
    if handler != nil {
        return handler, nil
    }

    if err := config.validate(); err != nil {
        return nil, err
    }

    // Only promote extesions using the Tus-Extension header which are implemented
    extensions := "creation,creation-with-upload"
    if config.StoreComposer.UsesTerminater {
        extensions += ",termination"
    }
    if config.StoreComposer.UsesConcater {
        extensions += ",concatenation"
    }
    if config.StoreComposer.UsesLengthDeferrer {
        extensions += ",creation-defer-length"
    }

    handler = &unroutedHandler{
        config:            config,
        composer:          config.StoreComposer,
        basePath:          config.BasePath,
        isBasePathAbs:     config.isAbs,
        CompleteUploads:   make(chan tusd.FileInfo),
        TerminatedUploads: make(chan tusd.FileInfo),
        UploadProgress:    make(chan tusd.FileInfo),
        CreatedUploads:    make(chan tusd.FileInfo),
        logger:            config.Logger,
        extensions:        extensions,
        Metrics:           newMetrics(),
    }

    return handler, nil
}

// newStoreComposerFromDataStore creates a new store composer and attempts to
// extract the extensions for the provided store. This is intended to be used
// for transitioning from data stores to composers.
func newStoreComposerFromDataStore(store tusd.DataStore) *tusd.StoreComposer {
    composer := tusd.NewStoreComposer()
    composer.UseCore(store)

    if mod, ok := store.(tusd.TerminaterDataStore); ok {
        composer.UseTerminater(mod)
    }
    if mod, ok := store.(tusd.FinisherDataStore); ok {
        composer.UseFinisher(mod)
    }
    if mod, ok := store.(tusd.LockerDataStore); ok {
        composer.UseLocker(mod)
    }
    if mod, ok := store.(tusd.GetReaderDataStore); ok {
        composer.UseGetReader(mod)
    }
    if mod, ok := store.(tusd.ConcaterDataStore); ok {
        composer.UseConcater(mod)
    }
    if mod, ok := store.(tusd.LengthDeferrerDataStore); ok {
        composer.UseLengthDeferrer(mod)
    }

    return composer
}
