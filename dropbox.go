/*
** Copyright (c) 2014 Arnaud Ysmal.  All Rights Reserved.
**
** Redistribution and use in source and binary forms, with or without
** modification, are permitted provided that the following conditions
** are met:
** 1. Redistributions of source code must retain the above copyright
**    notice, this list of conditions and the following disclaimer.
** 2. Redistributions in binary form must reproduce the above copyright
**    notice, this list of conditions and the following disclaimer in the
**    documentation and/or other materials provided with the distribution.
**
** THIS SOFTWARE IS PROVIDED BY THE AUTHOR ``AS IS'' AND ANY EXPRESS
** OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED
** WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
** DISCLAIMED. IN NO EVENT SHALL THE AUTHOR OR CONTRIBUTORS BE LIABLE
** FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
** DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
** SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION)
** HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT
** LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY
** OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF
** SUCH DAMAGE.
 */

// Package dropbox implements the Dropbox core API.
package dropbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"code.google.com/p/stacktic-goauth2/oauth"
)

// ErrNotAuth is the error returned when the OAuth token is not provided
var ErrNotAuth = errors.New("authentication required")

// Account represents information about the user account.
type Account struct {
	ReferralLink string `json:"referral_link"` // URL for referral.
	DisplayName  string `json:"display_name"`  // User name.
	UID          int    `json:"uid"`           // User account ID.
	Country      string `json:"country"`       // Country ISO code.
	QuotaInfo    struct {
		Shared int64 `json:"shared"` // Quota for shared files.
		Quota  int64 `json:"quota"`  // Quota in bytes.
		Normal int64 `json:"normal"` // Quota for non-shared files.
	} `json:"quota_info"`
}

// CopyRef represents thr reply of CopyRef.
type CopyRef struct {
	CopyRef string `json:"copy_ref"` // Reference to use on fileops/copy.
	Expires string `json:"expires"`  // Expiration date.
}

// DeltaPage represents the reply of delta.
type DeltaPage struct {
	Reset   bool         // if true the local state must be cleared.
	HasMore bool         // if true an other call to delta should be made.
	Cursor  string       // Tag of the current state.
	Entries []DeltaEntry // List of changed entries.
}

// DeltaEntry represents the the list of changes for a given path.
type DeltaEntry struct {
	Path  string // Path of this entry in lowercase.
	Entry *Entry // nil when this entry does not exists.
}

// DeltaPoll represents the reply of longpoll_delta.
type DeltaPoll struct {
	Changes bool `json:"changes"` // true if the polled path has changed.
	Backoff int  `json:"backoff"` // time in second before calling poll again.
}

// ChunkUploadResponse represents the reply of chunked_upload.
type ChunkUploadResponse struct {
	UploadID string `json:"upload_id"` // Unique ID of this upload.
	Offset   int    `json:"offset"`    // Size in bytes of already sent data.
	Expires  string `json:"expires"`   // Expiration time of this upload.
}

// Format of reply when http error code is not 200
// Format may be:
// {"error": "reason"}
// {"error": {"param": "reason"}}
type requestError struct {
	Error interface{} `json:"error"` // Description of this error.
}

const (
	// PollMinTimeout is the minimum timeout for longpoll
	PollMinTimeout = 30
	// PollMaxTimeout is the maximum timeout for longpoll
	PollMaxTimeout = 480
	// DefaultChunkSize is the maximum size of a file sendable using files_put.
	DefaultChunkSize = 4 * 1024 * 1024
	// MaxPutFileSize is the maximum size of a file sendable using files_put.
	MaxPutFileSize = 150 * 1024 * 1024
	// MetadataLimitMax is the maximum number of entries returned by metadata.
	MetadataLimitMax = 25000
	// MetadataLimitDefault is the default number of entries returned by metadata.
	MetadataLimitDefault = 10000
	// RevisionsLimitMax is the maximum number of revisions returned by revisions.
	RevisionsLimitMax = 1000
	// RevisionsLimitDefault is the default number of revisions returned by revisions.
	RevisionsLimitDefault = 10
	// SearchLimitMax is the maximum number of entries returned by search.
	SearchLimitMax = 1000
	// SearchLimitDefault is the default number of entries returned by search.
	SearchLimitDefault = 1000
	// DateFormat is the format to use when decoding a time.
	DateFormat = time.RFC1123Z
)

// Entry represents the metadata of a file or folder.
type Entry struct {
	Bytes       int     `json:"bytes"`        // Size of the file in bytes.
	ClientMtime string  `json:"client_mtime"` // Modification time set by the client when added.
	Contents    []Entry `json:"contents"`     // List of children for a directory.
	Hash        string  `json:"hash"`         // hash of this entry.
	Icon        string  `json:"icon"`         // Name of the icon displayed for this entry.
	IsDeleted   bool    `json:"is_deleted"`   // true if this entry was deleted.
	IsDir       bool    `json:"is_dir"`       // true if this entry is a directory.
	MimeType    string  `json:"mime_type"`    // MimeType of this entry.
	Modified    string  `json:"modified"`     // Date of last modification.
	Path        string  `json:"path"`         // Absolute path of this entry.
	Revision    string  `json:"rev"`          // Unique ID for this file revision.
	Root        string  `json:"root"`         // dropbox or sandbox.
	Size        string  `json:"size"`         // Size of the file humanized/localized.
	ThumbExists bool    `json:"thumb_exists"` // true if a thumbnail is available for this entry.
}

// Link for sharing a file.
type Link struct {
	Expires string `json:"expires"` // Expiration date of this link.
	URL     string `json:"url"`     // URL to share.
}

// Dropbox client.
type Dropbox struct {
	RootDirectory string          // dropbox or sandbox.
	Locale        string          // Locale send to the API to translate/format messages.
	APIURL        string          // Normal API URL.
	APIContentURL string          // URL for transferring files.
	APINotifyURL  string          // URL for realtime notification.
	Session       oauth.Transport // OAuth 2.0 session.
}

// NewDropbox returns a new Dropbox configured.
func NewDropbox() *Dropbox {
	return &Dropbox{
		RootDirectory: "dropbox", // dropbox or sandbox.
		Locale:        "en",
		APIURL:        "https://api.dropbox.com/1",
		APIContentURL: "https://api-content.dropbox.com/1",
		APINotifyURL:  "https://api-notify.dropbox.com/1",
		Session: oauth.Transport{
			Config: &oauth.Config{
				AuthURL:  "https://www.dropbox.com/1/oauth2/authorize",
				TokenURL: "https://api.dropbox.com/1/oauth2/token",
			},
		},
	}
}

// SetAppInfo sets the clientid (app_key) and clientsecret (app_secret).
// You have to register an application on https://www.dropbox.com/developers/apps.
func (db *Dropbox) SetAppInfo(clientid, clientsecret string) {
	db.Session.Config.ClientId = clientid
	db.Session.Config.ClientSecret = clientsecret
}

// SetAccessToken sets access token to avoid calling Auth method.
func (db *Dropbox) SetAccessToken(accesstoken string) {
	db.Session.Token = &oauth.Token{AccessToken: accesstoken}
}

// AccessToken returns the OAuth access token.
func (db *Dropbox) AccessToken() string {
	return db.Session.Token.AccessToken
}

// Auth displays the URL to authorize this application to connect to your account.
func (db *Dropbox) Auth() error {
	var code string

	fmt.Printf("Please visit:\n%s\nEnter the code: ",
		db.Session.Config.AuthCodeURL(""))
	fmt.Scanln(&code)
	_, err := db.Session.Exchange(code)
	return err
}

// CommitChunkedUpload ends the chunked upload by giving a name to the UploadID.
func (db *Dropbox) CommitChunkedUpload(uploadid, dst string, overwrite bool, parentRev string) (*Entry, error) {
	var err error
	var rawurl string
	var response *http.Response
	var params *url.Values
	var body []byte
	var rv Entry

	if dst[0] == '/' {
		dst = dst[1:]
	}

	params = &url.Values{
		"locale":    {db.Locale},
		"upload_id": {uploadid},
		"overwrite": {strconv.FormatBool(overwrite)},
	}
	if len(parentRev) != 0 {
		params.Set("parent_rev", parentRev)
	}
	rawurl = fmt.Sprintf("%s/commit_chunked_upload/%s/%s?%s", db.APIContentURL, db.RootDirectory, dst, params.Encode())

	if response, err = db.Session.Client().Post(rawurl, "", nil); err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if body, err = ioutil.ReadAll(response.Body); err == nil {
		err = json.Unmarshal(body, &rv)
	}
	return &rv, err
}

// ChunkedUpload sends a chunk with a maximum size of chunksize, if there is no session a new one is created.
func (db *Dropbox) ChunkedUpload(session *ChunkUploadResponse, input io.ReadCloser, chunksize int) (*ChunkUploadResponse, error) {
	var err error
	var rawurl string
	var cur ChunkUploadResponse
	var response *http.Response
	var body []byte
	var r *io.LimitedReader

	if chunksize <= 0 {
		chunksize = DefaultChunkSize
	} else if chunksize > MaxPutFileSize {
		chunksize = MaxPutFileSize
	}

	if session != nil {
		rawurl = fmt.Sprintf("%s/chunked_upload?upload_id=%s&offset=%d", db.APIContentURL, session.UploadID, session.Offset)
	} else {
		rawurl = fmt.Sprintf("%s/chunked_upload", db.APIContentURL)
	}
	r = &io.LimitedReader{R: input, N: int64(chunksize)}

	if response, err = db.Session.Client().Post(rawurl, "application/octet-stream", r); err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if body, err = ioutil.ReadAll(response.Body); err == nil {
		err = json.Unmarshal(body, &cur)
	}
	if r.N != 0 {
		err = io.EOF
	}
	return &cur, err
}

// UploadByChunk uploads data from the input reader to the dst path on Dropbox by sending chunks of chunksize.
func (db *Dropbox) UploadByChunk(input io.ReadCloser, chunksize int, dst string, overwrite bool, parentRev string) (*Entry, error) {
	var err error
	var cur *ChunkUploadResponse

	for err == nil {
		if cur, err = db.ChunkedUpload(cur, input, chunksize); err != nil && err != io.EOF {
			return nil, err
		}
	}
	return db.CommitChunkedUpload(cur.UploadID, dst, overwrite, parentRev)
}

// FilesPut uploads size bytes from the input reader to the dst path on Dropbox.
func (db *Dropbox) FilesPut(input io.ReadCloser, size int64, dst string, overwrite bool, parentRev string) (*Entry, error) {
	var err error
	var rawurl string
	var rv Entry
	var request *http.Request
	var response *http.Response
	var params *url.Values
	var body []byte

	if size > MaxPutFileSize {
		return nil, fmt.Errorf("could not upload files bigger than 150MB using this method, use UploadByChunk instead")
	}
	if dst[0] == '/' {
		dst = dst[1:]
	}

	params = &url.Values{"overwrite": {strconv.FormatBool(overwrite)}}
	if len(parentRev) != 0 {
		params.Set("parent_rev", parentRev)
	}
	rawurl = fmt.Sprintf("%s/files_put/%s/%s?%s", db.APIContentURL, db.RootDirectory, dst, params.Encode())

	if request, err = http.NewRequest("PUT", rawurl, input); err != nil {
		return nil, err
	}
	request.Header.Set("Content-Length", strconv.FormatInt(size, 10))
	if response, err = db.Session.Client().Do(request); err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if body, err = ioutil.ReadAll(response.Body); err == nil {
		err = json.Unmarshal(body, &rv)
	}
	return &rv, err
}

// UploadFile uploads the file located in the src path on the local disk to the dst path on Dropbox.
func (db *Dropbox) UploadFile(src, dst string, overwrite bool, parentRev string) (*Entry, error) {
	var err error
	var fd *os.File
	var fsize int64

	if fd, err = os.Open(src); err != nil {
		return nil, err
	}
	defer fd.Close()

	if fi, err := fd.Stat(); err == nil {
		fsize = fi.Size()
	} else {
		return nil, err
	}
	return db.FilesPut(fd, fsize, dst, overwrite, parentRev)
}

// Thumbnails gets a thumbnail for an image.
func (db *Dropbox) Thumbnails(src, format, size string) (io.ReadCloser, int64, *Entry, error) {
	var response *http.Response
	var rawurl string
	var err error
	var entry Entry

	switch format {
	case "":
		format = "jpeg"
	case "jpeg", "png":
		break
	default:
		return nil, 0, nil, fmt.Errorf("unsupported format '%s' must be jpeg or png", format)
	}
	switch size {
	case "":
		size = "s"
	case "xs", "s", "m", "l", "xl":
		break
	default:
		return nil, 0, nil, fmt.Errorf("unsupported size '%s' must be xs, s, m, l or xl", size)

	}
	if src[0] == '/' {
		src = src[1:]
	}
	rawurl = fmt.Sprintf("%s/thumbnails/%s/%s?format=%s&size=%s", db.APIContentURL, db.RootDirectory, src, format, size)
	if response, err = db.Session.Client().Get(rawurl); err != nil {
		return nil, 0, nil, err
	}
	switch response.StatusCode {
	case http.StatusNotFound:
		response.Body.Close()
		return nil, 0, nil, os.ErrNotExist
	case http.StatusUnsupportedMediaType:
		response.Body.Close()
		return nil, 0, nil, fmt.Errorf("the image located at '%s' cannot be converted to a thumbnail", src)
	}
	json.Unmarshal([]byte(response.Header.Get("x-dropbox-metadata")), &entry)
	return response.Body, response.ContentLength, &entry, err
}

// ThumbnailsToFile downloads the file located in the src path on the Dropbox to the dst file on the local disk.
func (db *Dropbox) ThumbnailsToFile(src, dst, format, size string) (*Entry, error) {
	var input io.ReadCloser
	var fd *os.File
	var err error
	var entry *Entry

	if fd, err = os.Create(dst); err != nil {
		return nil, err
	}
	defer fd.Close()

	if input, _, entry, err = db.Thumbnails(src, format, size); err != nil {
		os.Remove(dst)
		return nil, err
	}
	defer input.Close()
	if _, err = io.Copy(fd, input); err != nil {
		os.Remove(dst)
	}
	return entry, err
}

// Download requests the file located at src, the specific revision may be given.
// offset is used in case the download was interrupted.
// A io.ReadCloser to get the file ans its size is returned.
func (db *Dropbox) Download(src, rev string, offset int) (io.ReadCloser, int64, error) {
	var request *http.Request
	var response *http.Response
	var rawurl string
	var err error

	if src[0] == '/' {
		src = src[1:]
	}

	rawurl = fmt.Sprintf("%s/files/%s/%s", db.APIContentURL, db.RootDirectory, src)
	if len(rev) != 0 {
		rawurl += fmt.Sprintf("?rev=%s", rev)
	}
	if request, err = http.NewRequest("GET", rawurl, nil); err != nil {
		return nil, 0, err
	}
	if offset != 0 {
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	if response, err = db.Session.Client().Do(request); err != nil {
		return nil, 0, err
	}
	if response.StatusCode == http.StatusNotFound {
		response.Body.Close()
		return nil, 0, os.ErrNotExist
	}
	return response.Body, response.ContentLength, err
}

// DownloadToFileResume resumes the download of the file located in the src path on the Dropbox to the dst file on the local disk.
func (db *Dropbox) DownloadToFileResume(src, dst, rev string) error {
	var input io.ReadCloser
	var fi os.FileInfo
	var fd *os.File
	var offset int
	var err error

	if fd, err = os.OpenFile(dst, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err != nil {
		return err
	}
	defer fd.Close()
	if fi, err = fd.Stat(); err != nil {
		return err
	}
	offset = int(fi.Size())

	if input, _, err = db.Download(src, rev, offset); err != nil {
		return err
	}
	defer input.Close()
	_, err = io.Copy(fd, input)
	return err
}

// DownloadToFile downloads the file located in the src path on the Dropbox to the dst file on the local disk.
// If the destination file exists it will be truncated.
func (db *Dropbox) DownloadToFile(src, dst, rev string) error {
	var input io.ReadCloser
	var fd *os.File
	var err error

	if fd, err = os.Create(dst); err != nil {
		return err
	}
	defer fd.Close()

	if input, _, err = db.Download(src, rev, 0); err != nil {
		os.Remove(dst)
		return err
	}
	defer input.Close()
	if _, err = io.Copy(fd, input); err != nil {
		os.Remove(dst)
	}
	return err
}

func (db *Dropbox) doRequest(method, path string, params *url.Values, receiver interface{}) error {
	var body []byte
	var rawurl string
	var response *http.Response
	var request *http.Request
	var err error

	if params == nil {
		params = &url.Values{"locale": {db.Locale}}
	}
	rawurl = fmt.Sprintf("%s/%s?%s", db.APIURL, path, params.Encode())
	if request, err = http.NewRequest(method, rawurl, nil); err != nil {
		return err
	}
	if response, err = db.Session.Client().Do(request); err != nil {
		return err
	}
	defer response.Body.Close()
	if body, err = ioutil.ReadAll(response.Body); err != nil {
		return err
	}
	switch response.StatusCode {
	case http.StatusNotFound:
		return os.ErrNotExist
	case http.StatusBadRequest, http.StatusMethodNotAllowed:
		var reqerr requestError
		if err = json.Unmarshal(body, &reqerr); err != nil {
			return err
		}
		switch v := reqerr.Error.(type) {
		case string:
			return fmt.Errorf("%s", v)
		case map[string]interface{}:
			for param, reason := range v {
				if reasonstr, ok := reason.(string); ok {
					return fmt.Errorf("%s: %s", param, reasonstr)
				}
			}
			return fmt.Errorf("wrong parameter")
		default:
			return fmt.Errorf("request error HTTP code %d", response.StatusCode)
		}
	case http.StatusUnauthorized:
		return ErrNotAuth
	}
	err = json.Unmarshal(body, receiver)
	return err
}

// GetAccountInfo gets account information for the user currently authenticated.
func (db *Dropbox) GetAccountInfo() (*Account, error) {
	var rv Account

	err := db.doRequest("GET", "account/info", nil, &rv)
	return &rv, err
}

// Shares shares a file.
func (db *Dropbox) Shares(path string, shortURL bool) (*Link, error) {
	var rv Link
	var params *url.Values

	if shortURL {
		params = &url.Values{"short_url": {strconv.FormatBool(shortURL)}}
	}
	act := strings.Join([]string{"shares", db.RootDirectory, path}, "/")
	err := db.doRequest("POST", act, params, &rv)
	return &rv, err
}

// Media shares a file for streaming (direct access).
func (db *Dropbox) Media(path string) (*Link, error) {
	var rv Link

	act := strings.Join([]string{"media", db.RootDirectory, path}, "/")
	err := db.doRequest("POST", act, nil, &rv)
	return &rv, err
}

// Search searches the entries matching all the words contained in query in the given path.
// The maximum number of entries and whether to consider deleted file may be given.
func (db *Dropbox) Search(path, query string, fileLimit int, includeDeleted bool) (*[]Entry, error) {
	var rv []Entry
	var params *url.Values

	if fileLimit <= 0 || fileLimit > SearchLimitMax {
		fileLimit = SearchLimitDefault
	}
	params = &url.Values{
		"query":           {query},
		"file_limit":      {strconv.FormatInt(int64(fileLimit), 10)},
		"include_deleted": {strconv.FormatBool(includeDeleted)},
	}
	act := strings.Join([]string{"search", db.RootDirectory, path}, "/")
	err := db.doRequest("GET", act, params, &rv)
	return &rv, err
}

// Delta gets modifications since the cursor.
func (db *Dropbox) Delta(cursor, pathPrefix string) (*DeltaPage, error) {
	var rv DeltaPage
	var params *url.Values
	type deltaPageParser struct {
		Reset   bool                `json:"reset"`    // if true the local state must be cleared.
		HasMore bool                `json:"has_more"` // if true an other call to delta should be made.
		Cursor  string              `json:"cursor"`   // Tag of the current state.
		Entries [][]json.RawMessage `json:"entries"`  // List of changed entries.
	}
	var dpp deltaPageParser

	params = &url.Values{}
	if len(cursor) != 0 {
		params.Set("cursor", cursor)
	}
	if len(pathPrefix) != 0 {
		params.Set("path_prefix", pathPrefix)
	}
	err := db.doRequest("POST", "delta", params, &dpp)
	rv = DeltaPage{Reset: dpp.Reset, HasMore: dpp.HasMore, Cursor: dpp.Cursor}
	rv.Entries = make([]DeltaEntry, 0, len(dpp.Entries))
	for _, jentry := range dpp.Entries {
		var path string
		var entry Entry

		if len(jentry) != 2 {
			return nil, fmt.Errorf("malformed reply")
		}

		if err = json.Unmarshal(jentry[0], &path); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(jentry[1], &entry); err != nil {
			return nil, err
		}
		if entry.Path == "" {
			rv.Entries = append(rv.Entries, DeltaEntry{Path: path, Entry: nil})
		} else {
			rv.Entries = append(rv.Entries, DeltaEntry{Path: path, Entry: &entry})
		}
	}
	return &rv, err
}

// LongPollDelta waits for a notification to happen.
func (db *Dropbox) LongPollDelta(cursor string, timeout int) (*DeltaPoll, error) {
	var rv DeltaPoll
	var params *url.Values
	var body []byte
	var rawurl string
	var response *http.Response
	var err error
	var client http.Client

	params = &url.Values{}
	if timeout != 0 {
		if timeout < PollMinTimeout || timeout > PollMaxTimeout {
			return nil, fmt.Errorf("timeout out of range [%d; %d]", PollMinTimeout, PollMaxTimeout)
		}
		params.Set("timeout", strconv.FormatInt(int64(timeout), 10))
	}
	params.Set("cursor", cursor)
	rawurl = fmt.Sprintf("%s/longpoll_delta?%s", db.APINotifyURL, params.Encode())
	if response, err = client.Get(rawurl); err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if body, err = ioutil.ReadAll(response.Body); err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusBadRequest {
		var reqerr requestError
		if err = json.Unmarshal(body, &reqerr); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%s", reqerr.Error)
	}
	err = json.Unmarshal(body, &rv)
	return &rv, err
}

// Metadata gets the metadata for a file or a directory.
// If list is true and src is a directory, immediate child will be sent in the Contents field.
// If include_deleted is true, entries deleted will be sent.
// hash is the hash of the contents of a directory, it is used to avoid sending data when directory did not change.
// rev is the specific revision to get the metadata from.
// limit is the maximum number of entries requested.
func (db *Dropbox) Metadata(src string, list bool, includeDeleted bool, hash, rev string, limit int) (*Entry, error) {
	var rv Entry
	var params *url.Values

	if limit <= 0 {
		limit = MetadataLimitDefault
	} else if limit > MetadataLimitMax {
		limit = MetadataLimitMax
	}
	params = &url.Values{
		"list":            {strconv.FormatBool(list)},
		"include_deleted": {strconv.FormatBool(includeDeleted)},
		"file_limit":      {strconv.FormatInt(int64(limit), 10)},
	}
	if len(rev) != 0 {
		params.Set("rev", rev)
	}
	if len(hash) != 0 {
		params.Set("hash", hash)
	}

	act := strings.Join([]string{"metadata", db.RootDirectory, src}, "/")
	err := db.doRequest("GET", act, params, &rv)
	return &rv, err
}

// CopyRef gets a reference to a file.
// This reference can be used to copy this file to another user's Dropbox by passing it to the Copy method.
func (db *Dropbox) CopyRef(src string) (*CopyRef, error) {
	var rv CopyRef
	act := strings.Join([]string{"copy_ref", db.RootDirectory, src}, "/")
	err := db.doRequest("GET", act, nil, &rv)
	return &rv, err
}

// Revisions gets the list of revisions for a file.
func (db *Dropbox) Revisions(src string, revLimit int) (*[]Entry, error) {
	var rv []Entry
	if revLimit <= 0 {
		revLimit = RevisionsLimitDefault
	} else if revLimit > RevisionsLimitMax {
		revLimit = RevisionsLimitMax
	}
	act := strings.Join([]string{"revisions", db.RootDirectory, src}, "/")
	err := db.doRequest("GET", act,
		&url.Values{"rev_limit": {strconv.FormatInt(int64(revLimit), 10)}}, &rv)
	return &rv, err
}

// Restore restores a deleted file at the corresponding revision.
func (db *Dropbox) Restore(src string, rev string) (*Entry, error) {
	var rv Entry
	act := strings.Join([]string{"restore", db.RootDirectory, src}, "/")
	err := db.doRequest("POST", act, &url.Values{"rev": {rev}}, &rv)
	return &rv, err
}

// Copy copies a file.
// If isRef is true src must be a reference from CopyRef instead of a path.
func (db *Dropbox) Copy(src, dst string, isRef bool) (*Entry, error) {
	var rv Entry
	params := &url.Values{"root": {db.RootDirectory}, "to_path": {dst}}
	if isRef {
		params.Set("from_copy_ref", src)
	} else {
		params.Set("from_path", src)
	}
	err := db.doRequest("POST", "fileops/copy", params, &rv)
	return &rv, err
}

// CreateFolder creates a new directory.
func (db *Dropbox) CreateFolder(path string) (*Entry, error) {
	var rv Entry
	err := db.doRequest("POST", "fileops/create_folder",
		&url.Values{"root": {db.RootDirectory}, "path": {path}}, &rv)
	return &rv, err
}

// Delete removes a file or directory (it is a recursive delete).
func (db *Dropbox) Delete(path string) (*Entry, error) {
	var rv Entry
	err := db.doRequest("POST", "fileops/delete",
		&url.Values{"root": {db.RootDirectory}, "path": {path}}, &rv)
	return &rv, err
}

// Move moves a file or directory.
func (db *Dropbox) Move(src, dst string) (*Entry, error) {
	var rv Entry
	err := db.doRequest("POST", "fileops/move",
		&url.Values{"root": {db.RootDirectory},
			"from_path": {src},
			"to_path":   {dst}}, &rv)
	return &rv, err
}
