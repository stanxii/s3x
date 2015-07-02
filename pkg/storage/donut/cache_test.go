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
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or impliedc.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package donut

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"io/ioutil"
	"testing"
	"time"

	. "github.com/minio/check"
)

func TestCache(t *testing.T) { TestingT(t) }

type MyCacheSuite struct{}

var _ = Suite(&MyCacheSuite{})

var dc Cache

func (s *MyCacheSuite) SetUpSuite(c *C) {
	// no donut this time
	dc = NewCache(100000, time.Duration(1*time.Hour), "", nil)
	// testing empty cache
	buckets, err := dc.ListBuckets()
	c.Assert(err, IsNil)
	c.Assert(len(buckets), Equals, 0)
}

// test make bucket without name
func (s *MyCacheSuite) TestBucketWithoutNameFails(c *C) {
	// fail to create new bucket without a name
	err := dc.MakeBucket("", "private")
	c.Assert(err, Not(IsNil))

	err = dc.MakeBucket(" ", "private")
	c.Assert(err, Not(IsNil))
}

// test empty bucket
func (s *MyCacheSuite) TestEmptyBucket(c *C) {
	c.Assert(dc.MakeBucket("foo1", "private"), IsNil)
	// check if bucket is empty
	var resources BucketResourcesMetadata
	resources.Maxkeys = 1
	objectsMetadata, resources, err := dc.ListObjects("foo1", resources)
	c.Assert(err, IsNil)
	c.Assert(len(objectsMetadata), Equals, 0)
	c.Assert(resources.CommonPrefixes, DeepEquals, []string{})
	c.Assert(resources.IsTruncated, Equals, false)
}

// test bucket list
func (s *MyCacheSuite) TestMakeBucketAndList(c *C) {
	// create bucket
	err := dc.MakeBucket("foo2", "private")
	c.Assert(err, IsNil)

	// check bucket exists
	buckets, err := dc.ListBuckets()
	c.Assert(err, IsNil)
	c.Assert(len(buckets), Equals, 5)
	c.Assert(buckets[0].ACL, Equals, BucketACL("private"))
}

// test re-create bucket
func (s *MyCacheSuite) TestMakeBucketWithSameNameFails(c *C) {
	err := dc.MakeBucket("foo3", "private")
	c.Assert(err, IsNil)

	err = dc.MakeBucket("foo3", "private")
	c.Assert(err, Not(IsNil))
}

// test make multiple buckets
func (s *MyCacheSuite) TestCreateMultipleBucketsAndList(c *C) {
	// add a second bucket
	err := dc.MakeBucket("foo4", "private")
	c.Assert(err, IsNil)

	err = dc.MakeBucket("bar1", "private")
	c.Assert(err, IsNil)

	buckets, err := dc.ListBuckets()
	c.Assert(err, IsNil)

	c.Assert(len(buckets), Equals, 2)
	c.Assert(buckets[0].Name, Equals, "bar1")
	c.Assert(buckets[1].Name, Equals, "foo4")

	err = dc.MakeBucket("foobar1", "private")
	c.Assert(err, IsNil)

	buckets, err = dc.ListBuckets()
	c.Assert(err, IsNil)

	c.Assert(len(buckets), Equals, 3)
	c.Assert(buckets[2].Name, Equals, "foobar1")
}

// test object create without bucket
func (s *MyCacheSuite) TestNewObjectFailsWithoutBucket(c *C) {
	_, err := dc.CreateObject("unknown", "obj", "", "", 0, nil)
	c.Assert(err, Not(IsNil))
}

// test create object metadata
func (s *MyCacheSuite) TestNewObjectMetadata(c *C) {
	data := "Hello World"
	hasher := md5.New()
	hasher.Write([]byte(data))
	expectedMd5Sum := base64.StdEncoding.EncodeToString(hasher.Sum(nil))
	reader := ioutil.NopCloser(bytes.NewReader([]byte(data)))

	err := dc.MakeBucket("foo6", "private")
	c.Assert(err, IsNil)

	objectMetadata, err := dc.CreateObject("foo6", "obj", "application/json", expectedMd5Sum, int64(len(data)), reader)
	c.Assert(err, IsNil)
	c.Assert(objectMetadata.MD5Sum, Equals, hex.EncodeToString(hasher.Sum(nil)))
	c.Assert(objectMetadata.Metadata["contentType"], Equals, "application/json")
}

// test create object fails without name
func (s *MyCacheSuite) TestNewObjectFailsWithEmptyName(c *C) {
	_, err := dc.CreateObject("foo", "", "", "", 0, nil)
	c.Assert(err, Not(IsNil))
}

// test create object
func (s *MyCacheSuite) TestNewObjectCanBeWritten(c *C) {
	err := dc.MakeBucket("foo", "private")
	c.Assert(err, IsNil)

	data := "Hello World"

	hasher := md5.New()
	hasher.Write([]byte(data))
	expectedMd5Sum := base64.StdEncoding.EncodeToString(hasher.Sum(nil))
	reader := ioutil.NopCloser(bytes.NewReader([]byte(data)))

	actualMetadata, err := dc.CreateObject("foo", "obj", "application/octet-stream", expectedMd5Sum, int64(len(data)), reader)
	c.Assert(err, IsNil)
	c.Assert(actualMetadata.MD5Sum, Equals, hex.EncodeToString(hasher.Sum(nil)))

	var buffer bytes.Buffer
	size, err := dc.GetObject(&buffer, "foo", "obj")
	c.Assert(err, IsNil)
	c.Assert(size, Equals, int64(len(data)))
	c.Assert(buffer.Bytes(), DeepEquals, []byte(data))

	actualMetadata, err = dc.GetObjectMetadata("foo", "obj")
	c.Assert(err, IsNil)
	c.Assert(hex.EncodeToString(hasher.Sum(nil)), Equals, actualMetadata.MD5Sum)
	c.Assert(int64(len(data)), Equals, actualMetadata.Size)
}

// test list objects
func (s *MyCacheSuite) TestMultipleNewObjects(c *C) {
	c.Assert(dc.MakeBucket("foo5", "private"), IsNil)

	one := ioutil.NopCloser(bytes.NewReader([]byte("one")))

	_, err := dc.CreateObject("foo5", "obj1", "", "", int64(len("one")), one)
	c.Assert(err, IsNil)

	two := ioutil.NopCloser(bytes.NewReader([]byte("two")))
	_, err = dc.CreateObject("foo5", "obj2", "", "", int64(len("two")), two)
	c.Assert(err, IsNil)

	var buffer1 bytes.Buffer
	size, err := dc.GetObject(&buffer1, "foo5", "obj1")
	c.Assert(err, IsNil)
	c.Assert(size, Equals, int64(len([]byte("one"))))
	c.Assert(buffer1.Bytes(), DeepEquals, []byte("one"))

	var buffer2 bytes.Buffer
	size, err = dc.GetObject(&buffer2, "foo5", "obj2")
	c.Assert(err, IsNil)
	c.Assert(size, Equals, int64(len([]byte("two"))))

	c.Assert(buffer2.Bytes(), DeepEquals, []byte("two"))

	/// test list of objects

	// test list objects with prefix and delimiter
	var resources BucketResourcesMetadata
	resources.Prefix = "o"
	resources.Delimiter = "1"
	resources.Maxkeys = 10
	objectsMetadata, resources, err := dc.ListObjects("foo5", resources)
	c.Assert(err, IsNil)
	c.Assert(resources.IsTruncated, Equals, false)
	c.Assert(resources.CommonPrefixes[0], Equals, "obj1")

	// test list objects with only delimiter
	resources.Prefix = ""
	resources.Delimiter = "1"
	resources.Maxkeys = 10
	objectsMetadata, resources, err = dc.ListObjects("foo5", resources)
	c.Assert(err, IsNil)
	c.Assert(objectsMetadata[0].Object, Equals, "obj2")
	c.Assert(resources.IsTruncated, Equals, false)
	c.Assert(resources.CommonPrefixes[0], Equals, "obj1")

	// test list objects with only prefix
	resources.Prefix = "o"
	resources.Delimiter = ""
	resources.Maxkeys = 10
	objectsMetadata, resources, err = dc.ListObjects("foo5", resources)
	c.Assert(err, IsNil)
	c.Assert(resources.IsTruncated, Equals, false)
	c.Assert(objectsMetadata[0].Object, Equals, "obj1")
	c.Assert(objectsMetadata[1].Object, Equals, "obj2")

	three := ioutil.NopCloser(bytes.NewReader([]byte("three")))
	_, err = dc.CreateObject("foo5", "obj3", "", "", int64(len("three")), three)
	c.Assert(err, IsNil)

	var buffer bytes.Buffer
	size, err = dc.GetObject(&buffer, "foo5", "obj3")
	c.Assert(err, IsNil)
	c.Assert(size, Equals, int64(len([]byte("three"))))
	c.Assert(buffer.Bytes(), DeepEquals, []byte("three"))

	// test list objects with maxkeys
	resources.Prefix = "o"
	resources.Delimiter = ""
	resources.Maxkeys = 2
	objectsMetadata, resources, err = dc.ListObjects("foo5", resources)
	c.Assert(err, IsNil)
	c.Assert(resources.IsTruncated, Equals, true)
	c.Assert(len(objectsMetadata), Equals, 2)
}
