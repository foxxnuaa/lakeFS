package s3

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3iface"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/cznic/mathutil"
	"github.com/go-openapi/swag"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/scritchley/orc"
	"github.com/treeverse/lakefs/logging"
)

func generateOrc(t *testing.T, objs []InventoryObject) string {
	f, err := ioutil.TempFile("", "orctest")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = f.Close()
	}()
	schema, err := orc.ParseSchema("struct<bucket:string,key:string,size:int,last_modified_date:timestamp,e_tag:string>")
	if err != nil {
		t.Fatal(err)
	}
	w, err := orc.NewWriter(f, orc.SetSchema(schema), orc.SetStripeTargetSize(100))
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		err = w.Write(o.Bucket, o.Key, *o.Size, time.Unix(*o.LastModified, 0), *o.Checksum)
		if err != nil {
			t.Fatal(err)
		}
	}
	err = w.Close()
	if err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

func getS3Fake(t *testing.T) (s3iface.S3API, *httptest.Server) {
	backend := s3mem.New()
	faker := gofakes3.New(backend)
	ts := httptest.NewServer(faker.Server())
	// configure S3 client
	s3Config := &aws.Config{
		Credentials:      credentials.NewStaticCredentials("YOUR-ACCESSKEYID", "YOUR-SECRETACCESSKEY", ""),
		Endpoint:         aws.String(ts.URL),
		Region:           aws.String("eu-central-1"),
		DisableSSL:       aws.Bool(true),
		S3ForcePathStyle: aws.Bool(true),
	}
	newSession, err := session.NewSession(s3Config)
	if err != nil {
		t.Fatal(err)
	}
	return s3.New(newSession), ts
}

func uploadFile(t *testing.T, s3 s3iface.S3API, inventoryBucket string, inventoryFilename string, destBucket string, keys ...string) {
	objs := make([]InventoryObject, len(keys))
	for i, k := range keys {
		objs[i] = InventoryObject{
			Bucket:       destBucket,
			Key:          k,
			Size:         swag.Int64(500),
			LastModified: swag.Int64(time.Now().Unix()),
			Checksum:     swag.String("abcdefg"),
		}
	}
	localOrcFile := generateOrc(t, objs)
	f, err := os.Open(localOrcFile)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = f.Close()
	}()
	uploader := s3manager.NewUploaderWithClient(s3)
	_, err = uploader.Upload(&s3manager.UploadInput{
		Bucket: aws.String(inventoryBucket),
		Key:    aws.String(inventoryFilename),
		Body:   f,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func manifest(inventoryBucketName string, inventoryFilenames ...string) *Manifest {
	inventoryFiles := make([]inventoryFile, len(inventoryFilenames))
	for i, f := range inventoryFilenames {
		inventoryFiles[i] = inventoryFile{Key: f}
	}
	return &Manifest{
		URL:             "s3://my-bucket/manifest.json",
		SourceBucket:    "data-bucket",
		Files:           inventoryFiles,
		Format:          "ORC",
		inventoryBucket: inventoryBucketName,
	}
}

var (
	inventoryFileKeys = map[string][]string{"inventoryFile.orc": {"boo", "loo"}}
)

func TestInventoryReader(t *testing.T) {
	inventoryFileKeys["biggerFile.orc"] = make([]string, 0, 12500)
	for i := 0; i < 12500; i++ {
		inventoryFileKeys["biggerFile.orc"] = append(inventoryFileKeys["biggerFile.orc"], fmt.Sprintf("f%d", i))
	}
	svc, testServer := getS3Fake(t)
	defer testServer.Close()
	const inventoryBucketName = "inventory-bucket"
	_, err := svc.CreateBucket(&s3.CreateBucketInput{
		Bucket: aws.String(inventoryBucketName),
	})
	if err != nil {
		t.Fatal(err)
	}
	testdata := []struct {
		InventoryFilenames []string
	}{
		{InventoryFilenames: []string{"inventoryFile.orc"}},
		{InventoryFilenames: []string{"biggerFile.orc"}},
	}

	for _, test := range testdata {
		for _, inventoryFilename := range test.InventoryFilenames {
			uploadFile(t, svc, inventoryBucketName, inventoryFilename, "data-bucket", inventoryFileKeys[inventoryFilename]...)
		}
		m := manifest(inventoryBucketName, test.InventoryFilenames...)
		reader := NewInventoryReader(context.Background(), svc, logging.Default())
		for _, inventoryFilename := range test.InventoryFilenames {
			fileReader, err := reader.GetInventoryFileReader(m, inventoryFilename)
			if err != nil {
				t.Fatal(err)
			}
			res := make([]InventoryObject, 1000)
			offset := 0
			for {
				err = fileReader.Read(&res)
				for i := offset; i < len(res) && i < mathutil.Min(offset+1000, len(inventoryFileKeys[inventoryFilename])); i++ {
					if res[i-offset].Key != inventoryFileKeys[inventoryFilename][i] {
						t.Fatalf("result in index %d (index in batch: %d) different than expected. expected=%s, got=%s", i, i-offset, inventoryFileKeys[inventoryFilename][i], res[i-offset].Key)
					}
				}
				offset += len(res)
				if err != nil {
					t.Fatal(err)
				}
				if len(res) != 1000 {
					break
				}
			}
			if len(inventoryFileKeys[inventoryFilename]) != offset {
				t.Fatalf("read unexpected number of keys from inventory file %s. expected=%d, got=%d", inventoryFilename, len(inventoryFileKeys[inventoryFilename]), offset)
			}
			fileReader.Close()
		}
	}
}
