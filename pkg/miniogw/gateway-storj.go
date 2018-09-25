// Copyright (C) 2018 Storj Labs, Inc.
// See LICENSE for copying information.

package miniogw

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	minio "github.com/minio/minio/cmd"
	"github.com/minio/minio/pkg/auth"
	"github.com/minio/minio/pkg/hash"
	"github.com/zeebo/errs"
	monkit "gopkg.in/spacemonkeygo/monkit.v2"

	"storj.io/storj/pkg/paths"
	"storj.io/storj/pkg/ranger"
	"storj.io/storj/pkg/storage/buckets"
	"storj.io/storj/pkg/storage/meta"
	"storj.io/storj/pkg/storage/objects"
	"storj.io/storj/pkg/utils"
	"storj.io/storj/storage"
)

var (
	mon = monkit.Package()
	//Error is the errs class of standard End User Client errors
	Error = errs.Class("Storj Gateway error")
)

// NewStorjGateway creates a *Storj object from an existing ObjectStore
func NewStorjGateway(bs buckets.Store) *Storj {
	return &Storj{bs: bs, multipart: NewMultipartUploads()}
}

//Storj is the implementation of a minio cmd.Gateway
type Storj struct {
	bs        buckets.Store
	multipart *MultipartUploads
}

// Name implements cmd.Gateway
func (s *Storj) Name() string {
	return "storj"
}

// NewGatewayLayer implements cmd.Gateway
func (s *Storj) NewGatewayLayer(creds auth.Credentials) (
	minio.ObjectLayer, error) {
	return &storjObjects{storj: s}, nil
}

// Production implements cmd.Gateway
func (s *Storj) Production() bool {
	return false
}

type storjObjects struct {
	minio.GatewayUnsupported
	storj *Storj
}

func (s *storjObjects) DeleteBucket(ctx context.Context, bucket string) (err error) {
	defer mon.Task()(&ctx)(&err)
	_, err = s.storj.bs.Get(ctx, bucket)
	if err != nil {
		if storage.ErrKeyNotFound.Has(err) {
			return minio.BucketNotFound{Bucket: bucket}
		}
		return err
	}
	o, err := s.storj.bs.GetObjectStore(ctx, bucket)
	if err != nil {
		return err
	}
	items, _, err := o.List(ctx, nil, nil, nil, true, 1, meta.None)
	if err != nil {
		return err
	}
	if len(items) > 0 {
		return minio.BucketNotEmpty{Bucket: bucket}
	}
	return s.storj.bs.Delete(ctx, bucket)
}

func (s *storjObjects) DeleteObject(ctx context.Context, bucket, object string) (err error) {
	defer mon.Task()(&ctx)(&err)
	o, err := s.storj.bs.GetObjectStore(ctx, bucket)
	if err != nil {
		return err
	}
	err = o.Delete(ctx, paths.New(object))
	if storage.ErrKeyNotFound.Has(err) {
		err = minio.ObjectNotFound{Bucket: bucket, Object: object}
	}
	return err
}

func (s *storjObjects) GetBucketInfo(ctx context.Context, bucket string) (
	bucketInfo minio.BucketInfo, err error) {
	defer mon.Task()(&ctx)(&err)
	meta, err := s.storj.bs.Get(ctx, bucket)

	if err != nil {
		if storage.ErrKeyNotFound.Has(err) {
			return bucketInfo, minio.BucketNotFound{Bucket: bucket}
		}
		return bucketInfo, err
	}

	return minio.BucketInfo{Name: bucket, Created: meta.Created}, nil
}

func (s *storjObjects) getObject(ctx context.Context, bucket, object string) (rr ranger.Ranger, err error) {
	defer mon.Task()(&ctx)(&err)
	o, err := s.storj.bs.GetObjectStore(ctx, bucket)
	if err != nil {
		return nil, err
	}

	rr, _, err = o.Get(ctx, paths.New(object))

	return rr, err
}

func (s *storjObjects) GetObject(ctx context.Context, bucket, object string,
	startOffset int64, length int64, writer io.Writer, etag string) (err error) {
	defer mon.Task()(&ctx)(&err)

	rr, err := s.getObject(ctx, bucket, object)
	if err != nil {
		return err
	}

	if length == -1 {
		length = rr.Size() - startOffset
	}

	r, err := rr.Range(ctx, startOffset, length)
	if err != nil {
		return err
	}
	defer utils.LogClose(r)

	_, err = io.Copy(writer, r)

	return err
}

func (s *storjObjects) GetObjectInfo(ctx context.Context, bucket,
	object string) (objInfo minio.ObjectInfo, err error) {
	defer mon.Task()(&ctx)(&err)
	o, err := s.storj.bs.GetObjectStore(ctx, bucket)
	if err != nil {
		return minio.ObjectInfo{}, err
	}
	m, err := o.Meta(ctx, paths.New(object))
	if err != nil {
		if storage.ErrKeyNotFound.Has(err) {
			return objInfo, minio.ObjectNotFound{
				Bucket: bucket,
				Object: object,
			}
		}

		return objInfo, err
	}
	return minio.ObjectInfo{
		Name:        object,
		Bucket:      bucket,
		ModTime:     m.Modified,
		Size:        m.Size,
		ETag:        m.Checksum,
		ContentType: m.ContentType,
		UserDefined: m.UserDefined,
	}, err
}

func (s *storjObjects) ListBuckets(ctx context.Context) (
	bucketItems []minio.BucketInfo, err error) {
	defer mon.Task()(&ctx)(&err)
	startAfter := ""
	var items []buckets.ListItem
	for {
		moreItems, more, err := s.storj.bs.List(ctx, startAfter, "", 0)
		if err != nil {
			return nil, err
		}
		items = append(items, moreItems...)
		if !more {
			break
		}
		startAfter = moreItems[len(moreItems)-1].Bucket
	}
	bucketItems = make([]minio.BucketInfo, len(items))
	for i, item := range items {
		bucketItems[i].Name = item.Bucket
		bucketItems[i].Created = item.Meta.Created
	}
	return bucketItems, err
}

func (s *storjObjects) ListObjects(ctx context.Context, bucket, prefix, marker, delimiter string, maxKeys int) (result minio.ListObjectsInfo, err error) {
	defer mon.Task()(&ctx)(&err)

	if delimiter != "" && delimiter != "/" {
		return minio.ListObjectsInfo{}, Error.New("delimiter %s not supported", delimiter)
	}

	startAfter := paths.New(marker)
	recursive := delimiter == ""

	var objects []minio.ObjectInfo
	var prefixes []string
	o, err := s.storj.bs.GetObjectStore(ctx, bucket)
	if err != nil {
		return minio.ListObjectsInfo{}, err
	}
	items, more, err := o.List(ctx, paths.New(prefix), startAfter, nil, recursive, maxKeys, meta.All)
	if err != nil {
		return result, err
	}
	if len(items) > 0 {
		for _, item := range items {
			path := item.Path
			if recursive {
				path = path.Prepend(prefix)
			}
			if item.IsPrefix {
				prefixes = append(prefixes, path.String()+"/")
				continue
			}
			objects = append(objects, minio.ObjectInfo{
				Bucket:      bucket,
				IsDir:       false,
				Name:        path.String(),
				ModTime:     item.Meta.Modified,
				Size:        item.Meta.Size,
				ContentType: item.Meta.ContentType,
				UserDefined: item.Meta.UserDefined,
				ETag:        item.Meta.Checksum,
			})
		}
		startAfter = items[len(items)-1].Path
	}

	result = minio.ListObjectsInfo{
		IsTruncated: more,
		Objects:     objects,
		Prefixes:    prefixes,
	}
	if more {
		result.NextMarker = startAfter.String()
	}

	return result, err
}

// ListObjectsV2 - Not implemented stub
func (s *storjObjects) ListObjectsV2(ctx context.Context, bucket, prefix, continuationToken, delimiter string, maxKeys int, fetchOwner bool, startAfter string) (result minio.ListObjectsV2Info, err error) {
	defer mon.Task()(&ctx)(&err)

	if delimiter != "" && delimiter != "/" {
		return minio.ListObjectsV2Info{ContinuationToken: continuationToken}, Error.New("delimiter %s not supported", delimiter)
	}

	recursive := delimiter == ""
	var nextContinuationToken string

	var startAfterPath paths.Path
	if continuationToken != "" {
		startAfterPath = paths.New(continuationToken)
	}
	if startAfterPath == nil && startAfter != "" {
		startAfterPath = paths.New(startAfter)
	}

	var objects []minio.ObjectInfo
	var prefixes []string
	o, err := s.storj.bs.GetObjectStore(ctx, bucket)
	if err != nil {
		return minio.ListObjectsV2Info{ContinuationToken: continuationToken}, err
	}
	items, more, err := o.List(ctx, paths.New(prefix), startAfterPath, nil, recursive, maxKeys, meta.All)
	if err != nil {
		return result, err
	}

	if len(items) > 0 {
		for _, item := range items {
			path := item.Path
			if recursive {
				path = path.Prepend(prefix)
			}
			if item.IsPrefix {
				prefixes = append(prefixes, path.String()+"/")
				continue
			}
			objects = append(objects, minio.ObjectInfo{
				Bucket:      bucket,
				IsDir:       false,
				Name:        path.String(),
				ModTime:     item.Meta.Modified,
				Size:        item.Meta.Size,
				ContentType: item.Meta.ContentType,
				UserDefined: item.Meta.UserDefined,
				ETag:        item.Meta.Checksum,
			})
		}

		nextContinuationToken = items[len(items)-1].Path.String() + "\x00"
	}

	result = minio.ListObjectsV2Info{
		IsTruncated:       more,
		ContinuationToken: continuationToken,
		Objects:           objects,
		Prefixes:          prefixes,
	}
	if more {
		result.NextContinuationToken = nextContinuationToken
	}

	return result, err
}

func (s *storjObjects) MakeBucketWithLocation(ctx context.Context,
	bucket string, location string) (err error) {
	defer mon.Task()(&ctx)(&err)
	// TODO: This current strategy of calling bs.Get
	// to check if a bucket exists, then calling bs.Put
	// if not, can create a race condition if two people
	// call MakeBucketWithLocation at the same time and
	// therefore try to Put a bucket at the same time.
	// The reason for the Get call to check if the
	// bucket already exists is to match S3 CLI behavior.
	_, err = s.storj.bs.Get(ctx, bucket)
	if err == nil {
		return minio.BucketAlreadyExists{Bucket: bucket}
	}
	if !storage.ErrKeyNotFound.Has(err) {
		return err
	}
	_, err = s.storj.bs.Put(ctx, bucket)
	return err
}

func (s *storjObjects) CopyObject(ctx context.Context, srcBucket, srcObject, destBucket,
	destObject string, srcInfo minio.ObjectInfo) (objInfo minio.ObjectInfo, err error) {
	defer mon.Task()(&ctx)(&err)

	rr, err := s.getObject(ctx, srcBucket, srcObject)
	if err != nil {
		return objInfo, err
	}

	r, err := rr.Range(ctx, 0, rr.Size())
	if err != nil {
		return objInfo, err
	}

	defer utils.LogClose(r)

	serMetaInfo := objects.SerializableMeta{
		ContentType: srcInfo.ContentType,
		UserDefined: srcInfo.UserDefined,
	}

	return s.putObject(ctx, destBucket, destObject, r, serMetaInfo)
}

// SetupCloseHandler creates a 'listener' on a new goroutine which will notify the
// program if it receives an interrupt from the OS. We then handle this by calling
// our clean up procedure and exiting the program.
func SetupCloseHandler(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		fmt.Println("\r- Ctrl+C pressed in Terminal")
		signal.Stop(c)
		cancel()
		os.Exit(0)
	}()
}

func (s *storjObjects) putObject(ctx context.Context, bucket, object string, r io.Reader,
	meta objects.SerializableMeta) (objInfo minio.ObjectInfo, err error) {
	defer mon.Task()(&ctx)(&err)

	ctx, cancel := context.WithCancel(ctx)

	log.Println("HELELLJLKSDFJLSJDFJSKFDJLSJFLSJFLSJFLJFLDSKJFKLDS:JFKLSDJFKLSJFLKDSFJKLDSJFKLSFJKLSDFJKLSJFKLSDJFKLSDJFKLSJDFKLDSJFKLSDFJSEFLKJ ")
	/* create a signal of type os.Signal */
	c := make(chan os.Signal, 0x01)

	/* register for the os signals */
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-c
		log.Println("cancelling .......")
		signal.Stop(c)
		cancel()
		return
	}()

	go func(ctx context.Context, bucket, object string) {
		<-ctx.Done()
		log.Println("ctx.Done() cancelling .......")
		//err = s.DeleteObject(ctx, bucket, object)
		if err != nil {
			return
		}
	}(ctx, bucket, object)

	// setting zero value means the object never expires
	expTime := time.Time{}
	o, err := s.storj.bs.GetObjectStore(ctx, bucket)
	if err != nil {
		return minio.ObjectInfo{}, err
	}
	m, err := o.Put(ctx, paths.New(object), r, meta, expTime)
	return minio.ObjectInfo{
		Name:        object,
		Bucket:      bucket,
		ModTime:     m.Modified,
		Size:        m.Size,
		ETag:        m.Checksum,
		ContentType: m.ContentType,
		UserDefined: m.UserDefined,
	}, err
}

func (s *storjObjects) PutObject(ctx context.Context, bucket, object string,
	data *hash.Reader, metadata map[string]string) (objInfo minio.ObjectInfo,
	err error) {

	defer mon.Task()(&ctx)(&err)
	tempContType := metadata["content-type"]
	delete(metadata, "content-type")
	//metadata serialized
	serMetaInfo := objects.SerializableMeta{
		ContentType: tempContType,
		UserDefined: metadata,
	}

	return s.putObject(ctx, bucket, object, data, serMetaInfo)
}

func (s *storjObjects) Shutdown(ctx context.Context) (err error) {
	defer mon.Task()(&ctx)(&err)
	return nil
}

func (s *storjObjects) StorageInfo(context.Context) minio.StorageInfo {
	return minio.StorageInfo{}
}
