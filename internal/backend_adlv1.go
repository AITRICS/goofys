// Copyright 2019 Databricks
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

package internal

import (
	. "github.com/AITRICS/goofys/api/common"

	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jacobsa/fuse"
	uuid "github.com/satori/go.uuid"
	"github.com/sirupsen/logrus"

	adl "github.com/Azure/azure-sdk-for-go/services/datalake/store/2016-11-01/filesystem"
	"github.com/Azure/go-autorest/autorest"
)

type ADLv1 struct {
	cap Capabilities

	flags  *FlagStorage
	config *ADLv1Config

	client  *adl.Client
	account string
	// ADLv1 doesn't actually have the concept of buckets (defined
	// by me as a top level container that can be created with
	// existing credentials). We could create new adl filesystems
	// but that seems more involved. This bucket is more like a
	// backend level prefix mostly to ease testing
	bucket string
}

type ADLv1Err struct {
	RemoteException struct {
		Exception     string
		Message       string
		JavaClassName string
	}
	resp *http.Response
}

func (err ADLv1Err) Error() string {
	return fmt.Sprintf("%v %v", err.resp.Status, err.RemoteException)
}

const ADL1_REQUEST_ID = "X-Ms-Request-Id"

var adls1Log = GetLogger("adlv1")

type ADLv1MultipartBlobCommitInput struct {
	Size uint64
}

func IsADLv1Endpoint(endpoint string) bool {
	return strings.HasPrefix(endpoint, "adl://")
	//return strings.HasSuffix(endpoint, ".azuredatalakestore.net")
}

func adlLogResp(level logrus.Level, r *http.Response) {
	if adls1Log.IsLevelEnabled(level) {
		op := r.Request.URL.Query().Get("op")
		requestId := r.Request.Header.Get(ADL1_REQUEST_ID)
		respId := r.Header.Get(ADL1_REQUEST_ID)
		adls1Log.Logf(level, "%v %v %v %v %v", op, r.Request.URL.String(),
			requestId, r.Status, respId)
	}
}

func NewADLv1(bucket string, flags *FlagStorage, config *ADLv1Config) (*ADLv1, error) {
	parts := strings.SplitN(config.Endpoint, ".", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("Invalid endpoint: %v", config.Endpoint)
	}

	LogRequest := func(p autorest.Preparer) autorest.Preparer {
		return autorest.PreparerFunc(func(r *http.Request) (*http.Request, error) {
			// the autogenerated permission bits are
			// incorrect, it should be a string in base 8
			// instead of base 10
			q := r.URL.Query()
			if perm := q.Get("permission"); perm != "" {
				perm, err := strconv.ParseInt(perm, 10, 32)
				if err == nil {
					q.Set("permission",
						fmt.Sprintf("0%o", perm))
					r.URL.RawQuery = q.Encode()
				}
			}

			u, _ := uuid.NewV4()
			r.Header.Add(ADL1_REQUEST_ID, u.String())

			if adls1Log.IsLevelEnabled(logrus.DebugLevel) {
				op := r.URL.Query().Get("op")
				requestId := r.Header.Get(ADL1_REQUEST_ID)
				adls1Log.Debugf("%v %v %v", op, r.URL.String(), requestId)
			}

			r, err := p.Prepare(r)
			if err != nil {
				log.Error(err)
			}
			return r, err
		})
	}

	LogResponse := func(p autorest.Responder) autorest.Responder {
		return autorest.ResponderFunc(func(r *http.Response) error {
			adlLogResp(logrus.DebugLevel, r)
			err := p.Respond(r)
			if err != nil {
				log.Error(err)
			}
			return err
		})
	}

	adlClient := adl.NewClient()
	adlClient.BaseClient.Client.Authorizer = config.Authorizer
	adlClient.BaseClient.Client.RequestInspector = LogRequest
	adlClient.BaseClient.Client.ResponseInspector = LogResponse
	adlClient.BaseClient.AdlsFileSystemDNSSuffix = parts[1]
	adlClient.BaseClient.Sender.(*http.Client).Transport = GetHTTPTransport()

	b := &ADLv1{
		flags:   flags,
		config:  config,
		client:  &adlClient,
		account: parts[0],
		bucket:  bucket,
		cap: Capabilities{
			NoParallelMultipart: true,
			DirBlob:             true,
			Name:                "adl",
		},
	}

	return b, nil
}

func (b *ADLv1) Bucket() string {
	return b.bucket
}

func mapADLv1Error(resp *http.Response, err error, rawError bool) error {
	if resp == nil {
		if err != nil {
			return syscall.EAGAIN
		} else {
			return err
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		if rawError {
			decoder := json.NewDecoder(resp.Body)
			var adlErr ADLv1Err

			var err error
			if err = decoder.Decode(&adlErr); err == nil {
				adlErr.resp = resp
				return adlErr
			} else {
				adls1Log.Errorf("cannot parse error: %v", err)
				return syscall.EAGAIN
			}
		} else {
			err = mapHttpError(resp.StatusCode)
			if err != nil {
				return err
			} else {
				adlLogResp(logrus.ErrorLevel, resp)
				return syscall.EINVAL
			}
		}
	}
	return nil
}

func (b *ADLv1) path(key string) string {
	key = strings.TrimLeft(key, "/")
	if b.bucket != "" {
		if key != "" {
			key = b.bucket + "/" + key
		} else {
			key = b.bucket
		}
	}
	return key
}

func (b *ADLv1) Init(key string) error {
	res, err := b.client.GetFileStatus(context.TODO(), b.account, b.path(key), nil)
	err = mapADLv1Error(res.Response.Response, err, true)
	if adlErr, ok := err.(ADLv1Err); ok {
		if adlErr.RemoteException.Exception == "FileNotFoundException" {
			return nil
		}
	}
	return err
}

func (b *ADLv1) Capabilities() *Capabilities {
	return &b.cap
}

func adlv1LastModified(t int64) time.Time {
	return time.Unix(t/1000, t%1000000)
}

func adlv1FileStatus2BlobItem(f *adl.FileStatusProperties, key *string) BlobItemOutput {
	return BlobItemOutput{
		Key:          key,
		LastModified: PTime(adlv1LastModified(*f.ModificationTime)),
		Size:         uint64(*f.Length),
	}
}

func (b *ADLv1) HeadBlob(param *HeadBlobInput) (*HeadBlobOutput, error) {
	res, err := b.client.GetFileStatus(context.TODO(), b.account, b.path(param.Key), nil)
	err = mapADLv1Error(res.Response.Response, err, false)
	if err != nil {
		return nil, err
	}

	return &HeadBlobOutput{
		BlobItemOutput: adlv1FileStatus2BlobItem(res.FileStatus, &param.Key),
		IsDirBlob:      res.FileStatus.Type == "DIRECTORY",
	}, nil

}

func (b *ADLv1) appendToListResults(path string, recursive bool, startAfter string,
	maxKeys *uint32, prefixes []BlobPrefixOutput, items []BlobItemOutput) (adl.FileStatusesResult, []BlobPrefixOutput, []BlobItemOutput, error) {

	res, err := b.client.ListFileStatus(context.TODO(), b.account, b.path(path),
		nil, "", "", nil)
	err = mapADLv1Error(res.Response.Response, err, false)
	if err != nil {
		return adl.FileStatusesResult{}, nil, nil, err
	}

	if path != "" {
		if len(*res.FileStatuses.FileStatus) == 1 &&
			*(*res.FileStatuses.FileStatus)[0].PathSuffix == "" {
			// path is actually a file
			if !strings.HasSuffix(path, "/") {
				items = append(items,
					adlv1FileStatus2BlobItem(&(*res.FileStatuses.FileStatus)[0], &path))
			}
			return res, prefixes, items, nil
		}

		if !recursive {
			if strings.HasSuffix(path, "/") {
				// we listed for the dir object itself
				items = append(items, BlobItemOutput{
					Key: PString(path),
				})
			} else {
				prefixes = append(prefixes, BlobPrefixOutput{
					PString(path + "/"),
				})
			}
		}
	}

	path = strings.TrimRight(path, "/")

	if maxKeys != nil {
		*maxKeys -= uint32(len(*res.FileStatuses.FileStatus))
	}

	for _, i := range *res.FileStatuses.FileStatus {
		key := *i.PathSuffix
		if path != "" {
			key = path + "/" + key
		}

		if i.Type == "DIRECTORY" {
			if recursive {
				// we shouldn't generate prefixes if
				// it's a recursive listing
				items = append(items,
					adlv1FileStatus2BlobItem(&i, PString(key+"/")))

				_, prefixes, items, err = b.appendToListResults(key,
					recursive, "", maxKeys, prefixes, items)
			} else {
				prefixes = append(prefixes, BlobPrefixOutput{
					Prefix: PString(key + "/"),
				})
			}
		} else {
			items = append(items, adlv1FileStatus2BlobItem(&i, &key))
		}
	}

	return res, prefixes, items, nil
}

func (b *ADLv1) ListBlobs(param *ListBlobsInput) (*ListBlobsOutput, error) {
	var recursive bool
	if param.Delimiter == nil {
		// used by tests to cleanup (and also slurping, but
		// that's only enabled on S3 right now)
		recursive = true
		// cannot emulate these
		if param.ContinuationToken != nil || param.StartAfter != nil {
			return nil, syscall.ENOTSUP
		}
	} else if *param.Delimiter != "/" {
		return nil, syscall.ENOTSUP
	}
	continuationToken := param.ContinuationToken
	if param.StartAfter != nil {
		continuationToken = param.StartAfter
	}

	_, prefixes, items, err := b.appendToListResults(nilStr(param.Prefix),
		recursive, nilStr(continuationToken), param.MaxKeys, nil, nil)
	if err == fuse.ENOENT {
		err = nil
	} else if err != nil {
		return nil, err
	}

	return &ListBlobsOutput{
		Prefixes:    prefixes,
		Items:       items,
		IsTruncated: false,
	}, nil
}

func (b *ADLv1) DeleteBlob(param *DeleteBlobInput) (*DeleteBlobOutput, error) {
	res, err := b.client.Delete(context.TODO(), b.account, b.path(strings.TrimRight(param.Key, "/")), PBool(false))
	err = mapADLv1Error(res.Response.Response, err, false)
	if err != nil {
		return nil, err
	}
	if !*res.OperationResult {
		return nil, fuse.ENOENT
	}
	return &DeleteBlobOutput{}, nil
}

func (b *ADLv1) DeleteBlobs(param *DeleteBlobsInput) (ret *DeleteBlobsOutput, err error) {
	// if we delete a directory that's not empty, ADLv1 returns
	// 403. That can happen if we want to delete both "dir1" and
	// "dir1/file" but delete them in the wrong order for example
	// sort the blobs so the deepest tree are deleted first to
	// avoid this problem unfortunately because of this dependency
	// it's difficult to delete in parallel
	sort.Slice(param.Items, func(i, j int) bool {
		depth1 := len(strings.Split(strings.TrimRight(param.Items[i], "/"), "/"))
		depth2 := len(strings.Split(strings.TrimRight(param.Items[j], "/"), "/"))
		if depth1 != depth2 {
			return depth2 < depth1
		} else {
			return strings.Compare(param.Items[i], param.Items[j]) < 0
		}
	})

	for _, i := range param.Items {
		_, err := b.DeleteBlob(&DeleteBlobInput{i})
		if err != nil {
			return nil, err
		}

	}
	return &DeleteBlobsOutput{}, nil
}

func (b *ADLv1) RenameBlob(param *RenameBlobInput) (*RenameBlobOutput, error) {
	r, err := b.client.RenamePreparer(context.TODO(), b.account, b.path(param.Source),
		b.path(param.Destination))
	err = mapADLv1Error(nil, err, false)
	if err != nil {
		return nil, err
	}

	params := r.URL.Query()
	params.Add("renameoptions", "OVERWRITE")
	r.URL.RawQuery = params.Encode()

	resp, err := b.client.RenameSender(r)
	err = mapADLv1Error(resp, err, false)
	if err != nil {
		return nil, err
	}

	res, err := b.client.RenameResponder(resp)
	err = mapADLv1Error(resp, err, false)
	if err != nil {
		return nil, err
	}

	if !*res.OperationResult {
		// ADLv1 returns false if we try to rename a dir to a
		// file, or if the rename source doesn't exist. We
		// should have prevented renaming a dir to a file at
		// upper layer so this is probably the former

		// (the reverse, renaming a file to a directory works
		// in ADLv1 and is the same as moving the file into
		// the directory)
		return nil, fuse.ENOENT
	}

	return &RenameBlobOutput{}, nil
}

func (b *ADLv1) CopyBlob(param *CopyBlobInput) (*CopyBlobOutput, error) {
	return nil, syscall.ENOTSUP
}

func (b *ADLv1) GetBlob(param *GetBlobInput) (*GetBlobOutput, error) {
	var length *int64
	var offset *int64

	if param.Count != 0 {
		length = PInt64(int64(param.Count))
	}
	if param.Start != 0 {
		offset = PInt64(int64(param.Start))
	}

	var filesessionid *uuid.UUID
	if param.IfMatch != nil {
		b := make([]byte, 16)
		copy(b, []byte(*param.IfMatch))
		u, err := uuid.FromBytes(b)
		if err != nil {
			return nil, err
		}
		filesessionid = &u
	}

	resp, err := b.client.Open(context.TODO(), b.account, b.path(param.Key), length, offset,
		filesessionid)
	err = mapADLv1Error(resp.Response.Response, err, false)
	if err != nil {
		return nil, err
	}
	if resp.Value != nil {
		defer func() {
			if resp.Value != nil {
				(*resp.Value).Close()
			}
		}()
	}
	// WebHDFS specifies that Content-Length is returned but ADLv1
	// doesn't return it. Thankfully we never actually use it in
	// the context of GetBlobOutput

	var contentType *string
	// not very useful since ADLv1 always return application/octet-stream
	if val, ok := resp.Header["Content-Type"]; ok && len(val) != 0 {
		contentType = &val[len(val)-1]
	}

	res := GetBlobOutput{
		HeadBlobOutput: HeadBlobOutput{
			BlobItemOutput: BlobItemOutput{
				Key: &param.Key,
			},
			ContentType: contentType,
			IsDirBlob:   false,
		},
		Body: *resp.Value,
	}
	resp.Value = nil

	return &res, nil
}

func (b *ADLv1) PutBlob(param *PutBlobInput) (*PutBlobOutput, error) {
	if param.DirBlob {
		err := b.mkdir(param.Key)
		if err != nil {
			return nil, err
		}
	} else {
		res, err := b.client.Create(context.TODO(), b.account, b.path(param.Key),
			&ReadSeekerCloser{param.Body}, PBool(true), adl.CLOSE, nil,
			PInt32(int32(b.flags.FileMode)))
		err = mapADLv1Error(res.Response, err, false)
		if err != nil {
			return nil, err
		}
	}

	return &PutBlobOutput{}, nil
}

func (b *ADLv1) MultipartBlobBegin(param *MultipartBlobBeginInput) (*MultipartBlobCommitInput, error) {
	// ADLv1 doesn't have the concept of atomic replacement which
	// means that when we replace an object, readers may see
	// intermediate results. Here we implement MPU by first
	// sending a CREATE with 0 bytes and acquire a lease at the
	// same time.  much of these is not documented anywhere except
	// in the SDKs:
	// https://github.com/Azure/azure-data-lake-store-java/blob/f5c270b8cb2ac68536b2cb123d355a874cade34c/src/main/java/com/microsoft/azure/datalake/store/Core.java#L84
	leaseId, err := uuid.NewV4()
	if err != nil {
		return nil, err
	}

	res, err := b.client.Create(context.TODO(), b.account, b.path(param.Key),
		&ReadSeekerCloser{bytes.NewReader([]byte(""))}, PBool(true), adl.DATA, &leaseId,
		PInt32(int32(b.flags.FileMode)))
	err = mapADLv1Error(res.Response, err, false)
	if err != nil {
		return nil, err
	}

	return &MultipartBlobCommitInput{
		Key:         PString(b.path(param.Key)),
		UploadId:    PString(leaseId.String()),
		backendData: &ADLv1MultipartBlobCommitInput{},
	}, nil
}

func (b *ADLv1) uploadPart(param *MultipartBlobAddInput, offset uint64) error {
	leaseId, err := uuid.FromString(*param.Commit.UploadId)
	if err != nil {
		return err
	}

	res, err := b.client.Append(context.TODO(), b.account, *param.Commit.Key,
		&ReadSeekerCloser{param.Body}, PInt64(int64(offset-param.Size)), adl.DATA,
		&leaseId, &leaseId)
	err = mapADLv1Error(res.Response, err, true)
	if err != nil {
		if adlErr, ok := err.(ADLv1Err); ok {
			if adlErr.resp.StatusCode == 404 {
				// ADLv1 APPEND returns 404 if either:
				// the request payload is too large:
				// https://social.msdn.microsoft.com/Forums/azure/en-US/48e86ce8-79f8-4412-838f-8e2a60b5f387/notfound-error-on-call-to-data-lake-store-create?forum=AzureDataLake

				// or if a second concurrent stream is
				// created. The behavior is odd: seems
				// like the first stream will error
				// but the latter stream works fine
				err = fuse.EINVAL
				return err
			} else if adlErr.resp.StatusCode == 400 &&
				adlErr.RemoteException.Exception == "BadOffsetException" {
				appendErr := b.detectTransientError(param, offset)
				if appendErr == nil {
					return nil
				}
			}
			err = mapADLv1Error(adlErr.resp, err, false)
		}
	}
	return err
}

func (b *ADLv1) detectTransientError(param *MultipartBlobAddInput, offset uint64) error {
	leaseId, err := uuid.FromString(*param.Commit.UploadId)
	if err != nil {
		return err
	}
	res, err := b.client.Append(context.TODO(), b.account, *param.Commit.Key,
		&ReadSeekerCloser{bytes.NewReader([]byte(""))},
		PInt64(int64(offset)), adl.CLOSE, &leaseId, &leaseId)
	err = mapADLv1Error(res.Response, err, false)
	return err
}

func (b *ADLv1) MultipartBlobAdd(param *MultipartBlobAddInput) (*MultipartBlobAddOutput, error) {
	// APPEND with the expected offsets (so we can detect
	// concurrent updates to the same file and fail, in case lease
	// is for some reason broken on the server side

	var commitData *ADLv1MultipartBlobCommitInput
	var ok bool
	if commitData, ok = param.Commit.backendData.(*ADLv1MultipartBlobCommitInput); !ok {
		panic("Incorrect commit data type")
	}

	commitData.Size += param.Size
	err := b.uploadPart(param, commitData.Size)
	if err != nil {
		return nil, err
	}

	return &MultipartBlobAddOutput{}, nil
}

func (b *ADLv1) MultipartBlobAbort(param *MultipartBlobCommitInput) (*MultipartBlobAbortOutput, error) {
	// there's no such thing as abort, but at least we should release the lease
	// which technically is more like a commit than abort
	leaseId, err := uuid.FromString(*param.UploadId)
	if err != nil {
		return nil, err
	}
	res, err := b.client.Append(context.TODO(), b.account, *param.Key,
		&ReadSeekerCloser{bytes.NewReader([]byte(""))}, nil, adl.CLOSE, &leaseId, &leaseId)
	err = mapADLv1Error(res.Response, err, false)
	if err != nil {
		return nil, err
	}

	return &MultipartBlobAbortOutput{}, err
}

func (b *ADLv1) MultipartBlobCommit(param *MultipartBlobCommitInput) (*MultipartBlobCommitOutput, error) {
	var commitData *ADLv1MultipartBlobCommitInput
	var ok bool
	if commitData, ok = param.backendData.(*ADLv1MultipartBlobCommitInput); !ok {
		panic("Incorrect commit data type")
	}

	leaseId, err := uuid.FromString(*param.UploadId)
	if err != nil {
		return nil, err
	}
	res, err := b.client.Append(context.TODO(), b.account, *param.Key,
		&ReadSeekerCloser{bytes.NewReader([]byte(""))}, PInt64(int64(commitData.Size)),
		adl.CLOSE, &leaseId, &leaseId)
	err = mapADLv1Error(res.Response, err, false)
	if err == fuse.ENOENT {
		// either the blob was concurrently deleted or we got
		// another CREATE which broke our lease. Either way
		// technically we did finish uploading data so swallow
		// the error
		err = nil
	}
	if err != nil {
		return nil, err
	}

	return &MultipartBlobCommitOutput{}, nil
}

func (b *ADLv1) MultipartExpire(param *MultipartExpireInput) (*MultipartExpireOutput, error) {
	return nil, syscall.ENOTSUP
}

func (b *ADLv1) RemoveBucket(param *RemoveBucketInput) (*RemoveBucketOutput, error) {
	if b.bucket == "" {
		return nil, fuse.EINVAL
	}

	res, err := b.client.Delete(context.TODO(), b.account, b.path(""), PBool(false))
	err = mapADLv1Error(res.Response.Response, err, false)
	if err != nil {
		return nil, err
	}
	if !*res.OperationResult {
		return nil, fuse.ENOENT
	}

	return &RemoveBucketOutput{}, nil
}

func (b *ADLv1) MakeBucket(param *MakeBucketInput) (*MakeBucketOutput, error) {
	if b.bucket == "" {
		return nil, fuse.EINVAL
	}

	err := b.mkdir("")
	if err != nil {
		return nil, err
	}

	return &MakeBucketOutput{}, nil
}

func (b *ADLv1) mkdir(dir string) error {
	res, err := b.client.Mkdirs(context.TODO(), b.account, b.path(dir),
		PInt32(int32(b.flags.DirMode)))
	err = mapADLv1Error(res.Response.Response, err, true)
	if err != nil {
		return err
	}
	if !*res.OperationResult {
		return fuse.EEXIST
	}
	return nil
}
