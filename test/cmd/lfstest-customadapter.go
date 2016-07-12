// +build testtools

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strconv"
	"time"

	"github.com/github/git-lfs/api"
	"github.com/github/git-lfs/httputil"
	"github.com/github/git-lfs/progress"
	"github.com/github/git-lfs/tools"
)

// This test custom adapter just acts as a bridge for uploads/downloads
// in order to demonstrate & test the custom transfer adapter protocols
// All we actually do is relay the requests back to the normal storage URLs
// of our test server for simplicity, but this proves the principle
func main() {

	scanner := bufio.NewScanner(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)
	errWriter := bufio.NewWriter(os.Stderr)

	for scanner.Scan() {
		line := scanner.Text()
		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			errWriter.WriteString(fmt.Sprintf("Unable to parse request: %v\n", line))
			continue
		}

		switch req.Id {
		case "init":
			errWriter.WriteString(fmt.Sprintf("Initialised test custom adapter for %s\n", req.Operation))
			resp := &initResponse{}
			sendResponse(resp, writer)
		case "download":
			errWriter.WriteString(fmt.Sprintf("Received download request for %s\n", req.Oid))
			performDownload(req.Oid, req.Size, req.Action, writer, errWriter)
		case "upload":
			errWriter.WriteString(fmt.Sprintf("Received upload request for %s\n", req.Oid))
			performUpload(req.Oid, req.Size, req.Action, req.Path, writer, errWriter)
		case "terminate":
			errWriter.WriteString("Terminating test custom adapter gracefully.\n")
			break
		}
	}

}

func sendResponse(r interface{}, writer *bufio.Writer) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	// Line oriented JSON
	b = append(b, '\n')
	_, err = writer.Write(b)
	if err != nil {
		return err
	}
	writer.Flush()
	return nil
}

func sendTransferError(oid string, code int, message string, writer *bufio.Writer, errWriter *bufio.Writer) {
	resp := &transferResponse{"complete", oid, "", &transferError{code, message}}
	err := sendResponse(resp, writer)
	if err != nil {
		errWriter.WriteString(fmt.Sprintf("Unable to send transfer error: %v", err))
	}
}

func sendProgress(oid string, bytesSoFar int64, bytesSinceLast int, writer *bufio.Writer, errWriter *bufio.Writer) {
	resp := &progressResponse{"progress", oid, bytesSoFar, bytesSinceLast}
	err := sendResponse(resp, writer)
	if err != nil {
		errWriter.WriteString(fmt.Sprintf("Unable to send progress update: %v", err))
	}
}

func performDownload(oid string, size int64, a *action, writer *bufio.Writer, errWriter *bufio.Writer) {
	// We just use the URLs we're given, so we're just a proxy for the direct method
	// but this is enough to test intermediate custom adapters
	req, err := httputil.NewHttpRequest("GET", a.Href, a.Header)
	if err != nil {
		sendTransferError(oid, 2, err.Error(), writer, errWriter)
		return
	}
	res, err := httputil.DoHttpRequest(req, true)
	if err != nil {
		sendTransferError(oid, res.StatusCode, err.Error(), writer, errWriter)
		return
	}
	defer res.Body.Close()

	dlFile, err := ioutil.TempFile("", "lfscustomdl")
	if err != nil {
		sendTransferError(oid, 3, err.Error(), writer, errWriter)
		return
	}
	defer dlFile.Close()
	dlfilename := dlFile.Name()
	// Wrap callback to give name context
	cb := func(totalSize int64, readSoFar int64, readSinceLast int) error {
		sendProgress(oid, readSoFar, readSinceLast, writer, errWriter)
		return nil
	}
	_, err = tools.CopyWithCallback(dlFile, res.Body, res.ContentLength, cb)
	if err != nil {
		sendTransferError(oid, 4, fmt.Sprintf("cannot write data to tempfile %q: %v", dlfilename, err), writer, errWriter)
		os.Remove(dlfilename)
		return
	}
	if err := dlFile.Close(); err != nil {
		sendTransferError(oid, 5, fmt.Sprintf("can't close tempfile %q: %v", dlfilename, err), writer, errWriter)
		os.Remove(dlfilename)
		return
	}

	// completed
	complete := &transferResponse{"complete", oid, dlfilename, nil}
	err = sendResponse(complete, writer)
	if err != nil {
		errWriter.WriteString(fmt.Sprintf("Unable to send transfer error: %v", err))
	}
}

func performUpload(oid string, size int64, a *action, fromPath string, writer *bufio.Writer, errWriter *bufio.Writer) {
	// We just use the URLs we're given, so we're just a proxy for the direct method
	// but this is enough to test intermediate custom adapters
	req, err := httputil.NewHttpRequest("PUT", a.Href, a.Header)
	if err != nil {
		sendTransferError(oid, 2, err.Error(), writer, errWriter)
		return
	}

	if len(req.Header.Get("Content-Type")) == 0 {
		req.Header.Set("Content-Type", "application/octet-stream")
	}

	if req.Header.Get("Transfer-Encoding") == "chunked" {
		req.TransferEncoding = []string{"chunked"}
	} else {
		req.Header.Set("Content-Length", strconv.FormatInt(size, 10))
	}

	req.ContentLength = size

	f, err := os.OpenFile(fromPath, os.O_RDONLY, 0644)
	if err != nil {
		sendTransferError(oid, 3, fmt.Sprintf("Cannot read data from %q: %v", fromPath, err), writer, errWriter)
		return
	}
	defer f.Close()

	// Ensure progress callbacks made while uploading
	// Wrap callback to give name context
	cb := func(totalSize int64, readSoFar int64, readSinceLast int) error {
		sendProgress(oid, readSoFar, readSinceLast, writer, errWriter)
		return nil
	}
	var reader io.Reader
	reader = &progress.CallbackReader{
		C:         cb,
		TotalSize: size,
		Reader:    f,
	}

	req.Body = ioutil.NopCloser(reader)

	res, err := httputil.DoHttpRequest(req, true)
	if err != nil {
		sendTransferError(oid, res.StatusCode, fmt.Sprintf("Error uploading data for %s: %v", oid, err), writer, errWriter)
		return
	}

	if res.StatusCode > 299 {
		sendTransferError(oid, res.StatusCode, fmt.Sprintf("Invalid status for %s: %d", httputil.TraceHttpReq(req), res.StatusCode), writer, errWriter)
		return
	}

	io.Copy(ioutil.Discard, res.Body)
	res.Body.Close()

}

// Structs reimplemented so closer to a real external implementation
type header struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}
type action struct {
	Href      string            `json:"href"`
	Header    map[string]string `json:"header,omitempty"`
	ExpiresAt time.Time         `json:"expires_at,omitempty"`
}
type transferError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Combined request struct which can accept anything
type request struct {
	Id                  string  `json:"id"`
	Operation           string  `json:"operation"`
	Concurrent          bool    `json:"concurrent"`
	ConcurrentTransfers int     `json:"concurrenttransfers"`
	Oid                 string  `json:"oid"`
	Size                int64   `json:"size"`
	Path                string  `json:"path"`
	Action              *action `json:"action"`
}

type initResponse struct {
	Error *api.ObjectError `json:"error,omitempty"`
}
type transferResponse struct {
	Id    string         `json:"id"`
	Oid   string         `json:"oid"`
	Path  string         `json:"path,omitempty"` // always blank for upload
	Error *transferError `json:"error,omitempty"`
}
type progressResponse struct {
	Id             string `json:"id"`
	Oid            string `json:"oid"`
	BytesSoFar     int64  `json:"bytesSoFar"`
	BytesSinceLast int    `json:"bytesSinceLast"`
}
