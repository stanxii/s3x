/*
 * Minimalist Object Storage, (C) 2015 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package donut

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"io/ioutil"
	"log"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio/pkg/iodine"
	"github.com/minio/minio/pkg/storage/donut/trove"
)

// total Number of buckets allowed
const (
	totalBuckets = 100
)

// Cache - local variables
type Cache struct {
	storedBuckets    map[string]storedBucket
	lock             *sync.RWMutex
	objects          *trove.Cache
	multiPartObjects *trove.Cache
	maxSize          uint64
	expiration       time.Duration
	donut            Donut
}

// storedBucket saved bucket
type storedBucket struct {
	bucketMetadata   BucketMetadata
	objectMetadata   map[string]ObjectMetadata
	partMetadata     map[string]PartMetadata
	multiPartSession map[string]multiPartSession
}

// multiPartSession multipart session
type multiPartSession struct {
	totalParts int
	uploadID   string
	initiated  time.Time
}

type proxyWriter struct {
	writer       io.Writer
	writtenBytes []byte
}

func (r *proxyWriter) Write(p []byte) (n int, err error) {
	n, err = r.writer.Write(p)
	if err != nil {
		return
	}
	r.writtenBytes = append(r.writtenBytes, p[0:n]...)
	return
}

func newProxyWriter(w io.Writer) *proxyWriter {
	return &proxyWriter{writer: w, writtenBytes: nil}
}

// NewCache new cache
func NewCache(maxSize uint64, expiration time.Duration, donutName string, nodeDiskMap map[string][]string) Cache {
	c := Cache{}
	c.storedBuckets = make(map[string]storedBucket)
	c.objects = trove.NewCache(maxSize, expiration)
	c.multiPartObjects = trove.NewCache(0, time.Duration(0))
	c.objects.OnExpired = c.expiredObject
	c.multiPartObjects.OnExpired = c.expiredPart
	c.lock = new(sync.RWMutex)
	c.maxSize = maxSize
	c.expiration = expiration

	// set up cache expiration
	c.objects.ExpireObjects(time.Second * 5)
	c.donut, _ = NewDonut(donutName, nodeDiskMap)
	return c
}

// GetObject - GET object from cache buffer
func (cache Cache) GetObject(w io.Writer, bucket string, object string) (int64, error) {
	cache.lock.RLock()
	if !IsValidBucket(bucket) {
		cache.lock.RUnlock()
		return 0, iodine.New(BucketNameInvalid{Bucket: bucket}, nil)
	}
	if !IsValidObjectName(object) {
		cache.lock.RUnlock()
		return 0, iodine.New(ObjectNameInvalid{Object: object}, nil)
	}
	if _, ok := cache.storedBuckets[bucket]; ok == false {
		cache.lock.RUnlock()
		return 0, iodine.New(BucketNotFound{Bucket: bucket}, nil)
	}
	objectKey := bucket + "/" + object
	data, ok := cache.objects.Get(objectKey)
	if !ok {
		if cache.donut != nil {
			reader, size, err := cache.donut.GetObject(bucket, object)
			if err != nil {
				cache.lock.RUnlock()
				return 0, iodine.New(err, nil)
			}
			// new proxy writer to capture data read from disk
			pw := newProxyWriter(w)
			written, err := io.CopyN(pw, reader, size)
			if err != nil {
				cache.lock.RUnlock()
				return 0, iodine.New(err, nil)
			}
			cache.lock.RUnlock()
			/// cache object read from disk
			{
				cache.lock.Lock()
				ok := cache.objects.Set(objectKey, pw.writtenBytes)
				cache.lock.Unlock()
				pw.writtenBytes = nil
				go debug.FreeOSMemory()
				if !ok {
					return 0, iodine.New(InternalError{}, nil)
				}
			}
			return written, nil
		}
		cache.lock.RUnlock()
		return 0, iodine.New(ObjectNotFound{Object: object}, nil)
	}
	written, err := io.CopyN(w, bytes.NewBuffer(data), int64(cache.objects.Len(objectKey)))
	if err != nil {
		return 0, iodine.New(err, nil)
	}
	cache.lock.RUnlock()
	return written, nil
}

// GetPartialObject - GET object from cache buffer range
func (cache Cache) GetPartialObject(w io.Writer, bucket, object string, start, length int64) (int64, error) {
	errParams := map[string]string{
		"bucket": bucket,
		"object": object,
		"start":  strconv.FormatInt(start, 10),
		"length": strconv.FormatInt(length, 10),
	}
	cache.lock.RLock()
	if !IsValidBucket(bucket) {
		cache.lock.RUnlock()
		return 0, iodine.New(BucketNameInvalid{Bucket: bucket}, errParams)
	}
	if !IsValidObjectName(object) {
		cache.lock.RUnlock()
		return 0, iodine.New(ObjectNameInvalid{Object: object}, errParams)
	}
	if start < 0 {
		cache.lock.RUnlock()
		return 0, iodine.New(InvalidRange{
			Start:  start,
			Length: length,
		}, errParams)
	}
	objectKey := bucket + "/" + object
	data, ok := cache.objects.Get(objectKey)
	if !ok {
		if cache.donut != nil {
			reader, _, err := cache.donut.GetObject(bucket, object)
			if err != nil {
				cache.lock.RUnlock()
				return 0, iodine.New(err, nil)
			}
			if _, err := io.CopyN(ioutil.Discard, reader, start); err != nil {
				cache.lock.RUnlock()
				return 0, iodine.New(err, nil)
			}
			pw := newProxyWriter(w)
			written, err := io.CopyN(w, reader, length)
			if err != nil {
				cache.lock.RUnlock()
				return 0, iodine.New(err, nil)
			}
			cache.lock.RUnlock()
			{
				cache.lock.Lock()
				ok := cache.objects.Set(objectKey, pw.writtenBytes)
				cache.lock.Unlock()
				pw.writtenBytes = nil
				go debug.FreeOSMemory()
				if !ok {
					return 0, iodine.New(InternalError{}, nil)
				}
			}
			return written, nil
		}
		cache.lock.RUnlock()
		return 0, iodine.New(ObjectNotFound{Object: object}, nil)
	}
	written, err := io.CopyN(w, bytes.NewBuffer(data[start:]), length)
	if err != nil {
		return 0, iodine.New(err, nil)
	}
	cache.lock.RUnlock()
	return written, nil
}

// GetBucketMetadata -
func (cache Cache) GetBucketMetadata(bucket string) (BucketMetadata, error) {
	cache.lock.RLock()
	if !IsValidBucket(bucket) {
		cache.lock.RUnlock()
		return BucketMetadata{}, iodine.New(BucketNameInvalid{Bucket: bucket}, nil)
	}
	if _, ok := cache.storedBuckets[bucket]; ok == false {
		if cache.donut == nil {
			cache.lock.RUnlock()
			return BucketMetadata{}, iodine.New(BucketNotFound{Bucket: bucket}, nil)
		}
		bucketMetadata, err := cache.donut.GetBucketMetadata(bucket)
		if err != nil {
			cache.lock.RUnlock()
			return BucketMetadata{}, iodine.New(err, nil)
		}
		storedBucket := cache.storedBuckets[bucket]
		cache.lock.RUnlock()
		cache.lock.Lock()
		storedBucket.bucketMetadata = bucketMetadata
		cache.storedBuckets[bucket] = storedBucket
		cache.lock.Unlock()
	}
	cache.lock.RUnlock()
	return cache.storedBuckets[bucket].bucketMetadata, nil
}

// SetBucketMetadata -
func (cache Cache) SetBucketMetadata(bucket, acl string) error {
	cache.lock.RLock()
	if !IsValidBucket(bucket) {
		cache.lock.RUnlock()
		return iodine.New(BucketNameInvalid{Bucket: bucket}, nil)
	}
	if _, ok := cache.storedBuckets[bucket]; ok == false {
		cache.lock.RUnlock()
		return iodine.New(BucketNotFound{Bucket: bucket}, nil)
	}
	if strings.TrimSpace(acl) == "" {
		acl = "private"
	}
	cache.lock.RUnlock()
	cache.lock.Lock()
	m := make(map[string]string)
	m["acl"] = acl
	if cache.donut != nil {
		if err := cache.donut.SetBucketMetadata(bucket, m); err != nil {
			return iodine.New(err, nil)
		}
	}
	storedBucket := cache.storedBuckets[bucket]
	storedBucket.bucketMetadata.ACL = BucketACL(acl)
	cache.storedBuckets[bucket] = storedBucket
	cache.lock.Unlock()
	return nil
}

// isMD5SumEqual - returns error if md5sum mismatches, success its `nil`
func isMD5SumEqual(expectedMD5Sum, actualMD5Sum string) error {
	if strings.TrimSpace(expectedMD5Sum) != "" && strings.TrimSpace(actualMD5Sum) != "" {
		expectedMD5SumBytes, err := hex.DecodeString(expectedMD5Sum)
		if err != nil {
			return iodine.New(err, nil)
		}
		actualMD5SumBytes, err := hex.DecodeString(actualMD5Sum)
		if err != nil {
			return iodine.New(err, nil)
		}
		if !bytes.Equal(expectedMD5SumBytes, actualMD5SumBytes) {
			return iodine.New(errors.New("bad digest, md5sum mismatch"), nil)
		}
		return nil
	}
	return iodine.New(errors.New("invalid argument"), nil)
}

// CreateObject -
func (cache Cache) CreateObject(bucket, key, contentType, expectedMD5Sum string, size int64, data io.Reader) (ObjectMetadata, error) {
	if size > int64(cache.maxSize) {
		generic := GenericObjectError{Bucket: bucket, Object: key}
		return ObjectMetadata{}, iodine.New(EntityTooLarge{
			GenericObjectError: generic,
			Size:               strconv.FormatInt(size, 10),
			MaxSize:            strconv.FormatUint(cache.maxSize, 10),
		}, nil)
	}
	objectMetadata, err := cache.createObject(bucket, key, contentType, expectedMD5Sum, size, data)
	// free
	debug.FreeOSMemory()
	return objectMetadata, iodine.New(err, nil)
}

// createObject - PUT object to cache buffer
func (cache Cache) createObject(bucket, key, contentType, expectedMD5Sum string, size int64, data io.Reader) (ObjectMetadata, error) {
	cache.lock.RLock()
	if !IsValidBucket(bucket) {
		cache.lock.RUnlock()
		return ObjectMetadata{}, iodine.New(BucketNameInvalid{Bucket: bucket}, nil)
	}
	if !IsValidObjectName(key) {
		cache.lock.RUnlock()
		return ObjectMetadata{}, iodine.New(ObjectNameInvalid{Object: key}, nil)
	}
	if _, ok := cache.storedBuckets[bucket]; ok == false {
		cache.lock.RUnlock()
		return ObjectMetadata{}, iodine.New(BucketNotFound{Bucket: bucket}, nil)
	}
	storedBucket := cache.storedBuckets[bucket]
	// get object key
	objectKey := bucket + "/" + key
	if _, ok := storedBucket.objectMetadata[objectKey]; ok == true {
		cache.lock.RUnlock()
		return ObjectMetadata{}, iodine.New(ObjectExists{Object: key}, nil)
	}
	cache.lock.RUnlock()

	if contentType == "" {
		contentType = "application/octet-stream"
	}
	contentType = strings.TrimSpace(contentType)
	if strings.TrimSpace(expectedMD5Sum) != "" {
		expectedMD5SumBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(expectedMD5Sum))
		if err != nil {
			// pro-actively close the connection
			return ObjectMetadata{}, iodine.New(InvalidDigest{Md5: expectedMD5Sum}, nil)
		}
		expectedMD5Sum = hex.EncodeToString(expectedMD5SumBytes)
	}

	if cache.donut != nil {
		objMetadata, err := cache.donut.PutObject(bucket, key, expectedMD5Sum, data, map[string]string{"contentType": contentType})
		if err != nil {
			return ObjectMetadata{}, iodine.New(err, nil)
		}
		cache.lock.Lock()
		storedBucket.objectMetadata[objectKey] = objMetadata
		cache.storedBuckets[bucket] = storedBucket
		cache.lock.Unlock()
		return objMetadata, nil
	}
	// calculate md5
	hash := md5.New()

	var err error
	var totalLength int
	for err == nil {
		var length int
		byteBuffer := make([]byte, 1024*1024)
		length, err = data.Read(byteBuffer)
		// While hash.Write() wouldn't mind a Nil byteBuffer
		// It is necessary for us to verify this and break
		if length == 0 {
			break
		}
		hash.Write(byteBuffer[0:length])
		cache.lock.Lock()
		ok := cache.objects.Append(objectKey, byteBuffer[0:length])
		cache.lock.Unlock()
		if !ok {
			return ObjectMetadata{}, iodine.New(InternalError{}, nil)
		}
		totalLength += length
		go debug.FreeOSMemory()
	}
	if err != io.EOF {
		return ObjectMetadata{}, iodine.New(err, nil)
	}

	md5SumBytes := hash.Sum(nil)
	md5Sum := hex.EncodeToString(md5SumBytes)
	// Verify if the written object is equal to what is expected, only if it is requested as such
	if strings.TrimSpace(expectedMD5Sum) != "" {
		if err := isMD5SumEqual(strings.TrimSpace(expectedMD5Sum), md5Sum); err != nil {
			return ObjectMetadata{}, iodine.New(BadDigest{}, nil)
		}
	}

	m := make(map[string]string)
	m["contentType"] = contentType
	newObject := ObjectMetadata{
		Bucket: bucket,
		Object: key,

		Metadata: m,
		Created:  time.Now().UTC(),
		MD5Sum:   md5Sum,
		Size:     int64(totalLength),
	}

	cache.lock.Lock()
	storedBucket.objectMetadata[objectKey] = newObject
	cache.storedBuckets[bucket] = storedBucket
	cache.lock.Unlock()
	return newObject, nil
}

// MakeBucket - create bucket in cache
func (cache Cache) MakeBucket(bucketName, acl string) error {
	cache.lock.RLock()
	if len(cache.storedBuckets) == totalBuckets {
		cache.lock.RUnlock()
		return iodine.New(TooManyBuckets{Bucket: bucketName}, nil)
	}
	if !IsValidBucket(bucketName) {
		cache.lock.RUnlock()
		return iodine.New(BucketNameInvalid{Bucket: bucketName}, nil)
	}
	if !IsValidBucketACL(acl) {
		cache.lock.RUnlock()
		return iodine.New(InvalidACL{ACL: acl}, nil)
	}
	if _, ok := cache.storedBuckets[bucketName]; ok == true {
		cache.lock.RUnlock()
		return iodine.New(BucketExists{Bucket: bucketName}, nil)
	}
	cache.lock.RUnlock()

	if strings.TrimSpace(acl) == "" {
		// default is private
		acl = "private"
	}
	if cache.donut != nil {
		if err := cache.donut.MakeBucket(bucketName, BucketACL(acl)); err != nil {
			return iodine.New(err, nil)
		}
	}
	var newBucket = storedBucket{}
	newBucket.objectMetadata = make(map[string]ObjectMetadata)
	newBucket.multiPartSession = make(map[string]multiPartSession)
	newBucket.partMetadata = make(map[string]PartMetadata)
	newBucket.bucketMetadata = BucketMetadata{}
	newBucket.bucketMetadata.Name = bucketName
	newBucket.bucketMetadata.Created = time.Now().UTC()
	newBucket.bucketMetadata.ACL = BucketACL(acl)
	cache.lock.Lock()
	cache.storedBuckets[bucketName] = newBucket
	cache.lock.Unlock()
	return nil
}

// ListObjects - list objects from cache
func (cache Cache) ListObjects(bucket string, resources BucketResourcesMetadata) ([]ObjectMetadata, BucketResourcesMetadata, error) {
	cache.lock.RLock()
	defer cache.lock.RUnlock()
	if !IsValidBucket(bucket) {
		return nil, BucketResourcesMetadata{IsTruncated: false}, iodine.New(BucketNameInvalid{Bucket: bucket}, nil)
	}
	if !IsValidPrefix(resources.Prefix) {
		return nil, BucketResourcesMetadata{IsTruncated: false}, iodine.New(ObjectNameInvalid{Object: resources.Prefix}, nil)
	}
	if _, ok := cache.storedBuckets[bucket]; ok == false {
		return nil, BucketResourcesMetadata{IsTruncated: false}, iodine.New(BucketNotFound{Bucket: bucket}, nil)
	}
	var results []ObjectMetadata
	var keys []string
	if cache.donut != nil {
		listObjects, err := cache.donut.ListObjects(
			bucket,
			resources.Prefix,
			resources.Marker,
			resources.Delimiter,
			resources.Maxkeys,
		)
		if err != nil {
			return nil, BucketResourcesMetadata{IsTruncated: false}, iodine.New(err, nil)
		}
		resources.CommonPrefixes = listObjects.CommonPrefixes
		resources.IsTruncated = listObjects.IsTruncated
		if resources.IsTruncated && resources.IsDelimiterSet() {
			resources.NextMarker = results[len(results)-1].Object
		}
		for key := range listObjects.Objects {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			results = append(results, listObjects.Objects[key])
		}
		return results, resources, nil
	}
	storedBucket := cache.storedBuckets[bucket]
	for key := range storedBucket.objectMetadata {
		if strings.HasPrefix(key, bucket+"/") {
			key = key[len(bucket)+1:]
			if strings.HasPrefix(key, resources.Prefix) {
				if key > resources.Marker {
					keys = append(keys, key)
				}
			}
		}
	}
	if strings.TrimSpace(resources.Prefix) != "" {
		keys = TrimPrefix(keys, resources.Prefix)
	}
	var prefixes []string
	var filteredKeys []string
	if strings.TrimSpace(resources.Delimiter) != "" {
		filteredKeys = HasNoDelimiter(keys, resources.Delimiter)
		prefixes = HasDelimiter(keys, resources.Delimiter)
		prefixes = SplitDelimiter(prefixes, resources.Delimiter)
		prefixes = SortU(prefixes)
	} else {
		filteredKeys = keys
	}
	for _, commonPrefix := range prefixes {
		resources.CommonPrefixes = append(resources.CommonPrefixes, resources.Prefix+commonPrefix)
	}
	filteredKeys = RemoveDuplicates(filteredKeys)
	sort.Strings(filteredKeys)

	for _, key := range filteredKeys {
		if len(results) == resources.Maxkeys {
			resources.IsTruncated = true
			if resources.IsTruncated && resources.IsDelimiterSet() {
				resources.NextMarker = results[len(results)-1].Object
			}
			return results, resources, nil
		}
		object := storedBucket.objectMetadata[bucket+"/"+resources.Prefix+key]
		results = append(results, object)
	}
	resources.CommonPrefixes = RemoveDuplicates(resources.CommonPrefixes)
	sort.Strings(resources.CommonPrefixes)
	return results, resources, nil
}

// byBucketName is a type for sorting bucket metadata by bucket name
type byBucketName []BucketMetadata

func (b byBucketName) Len() int           { return len(b) }
func (b byBucketName) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }
func (b byBucketName) Less(i, j int) bool { return b[i].Name < b[j].Name }

// ListBuckets - List buckets from cache
func (cache Cache) ListBuckets() ([]BucketMetadata, error) {
	cache.lock.RLock()
	defer cache.lock.RUnlock()
	var results []BucketMetadata
	for _, bucket := range cache.storedBuckets {
		results = append(results, bucket.bucketMetadata)
	}
	sort.Sort(byBucketName(results))
	return results, nil
}

// GetObjectMetadata - get object metadata from cache
func (cache Cache) GetObjectMetadata(bucket, key string) (ObjectMetadata, error) {
	cache.lock.RLock()
	// check if bucket exists
	if !IsValidBucket(bucket) {
		cache.lock.RUnlock()
		return ObjectMetadata{}, iodine.New(BucketNameInvalid{Bucket: bucket}, nil)
	}
	if !IsValidObjectName(key) {
		cache.lock.RUnlock()
		return ObjectMetadata{}, iodine.New(ObjectNameInvalid{Object: key}, nil)
	}
	if _, ok := cache.storedBuckets[bucket]; ok == false {
		cache.lock.RUnlock()
		return ObjectMetadata{}, iodine.New(BucketNotFound{Bucket: bucket}, nil)
	}
	storedBucket := cache.storedBuckets[bucket]
	objectKey := bucket + "/" + key
	if objMetadata, ok := storedBucket.objectMetadata[objectKey]; ok == true {
		cache.lock.RUnlock()
		return objMetadata, nil
	}
	if cache.donut != nil {
		objMetadata, err := cache.donut.GetObjectMetadata(bucket, key)
		cache.lock.RUnlock()
		if err != nil {
			return ObjectMetadata{}, iodine.New(err, nil)
		}
		// update
		cache.lock.Lock()
		storedBucket.objectMetadata[objectKey] = objMetadata
		cache.lock.Unlock()
		return objMetadata, nil
	}
	cache.lock.RUnlock()
	return ObjectMetadata{}, iodine.New(ObjectNotFound{Object: key}, nil)
}

func (cache Cache) expiredObject(a ...interface{}) {
	cacheStats := cache.objects.Stats()
	log.Printf("CurrentSize: %d, CurrentItems: %d, TotalExpirations: %d",
		cacheStats.Bytes, cacheStats.Items, cacheStats.Expired)
	key := a[0].(string)
	// loop through all buckets
	for _, storedBucket := range cache.storedBuckets {
		delete(storedBucket.objectMetadata, key)
	}
	debug.FreeOSMemory()
}
