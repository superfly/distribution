package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"path"
	"reflect"
	"testing"

	"github.com/distribution/distribution/v3"
	"github.com/distribution/distribution/v3/reference"
	"github.com/distribution/distribution/v3/registry/storage/cache/memory"
	"github.com/distribution/distribution/v3/registry/storage/driver/testdriver"
	"github.com/distribution/distribution/v3/testutil"
	"github.com/opencontainers/go-digest"
)

// TestWriteSeek tests that the current file size can be
// obtained using Seek
func TestWriteSeek(t *testing.T) {
	ctx := context.Background()
	imageName, _ := reference.WithName("foo/bar")
	driver := testdriver.New()
	registry, err := NewRegistry(ctx, driver, BlobDescriptorCacheProvider(memory.NewInMemoryBlobDescriptorCacheProvider(memory.UnlimitedSize)), EnableDelete, EnableRedirect)
	if err != nil {
		t.Fatalf("error creating registry: %v", err)
	}
	repository, err := registry.Repository(ctx, imageName)
	if err != nil {
		t.Fatalf("unexpected error getting repo: %v", err)
	}
	bs := repository.Blobs(ctx)

	blobUpload, err := bs.Create(ctx)
	if err != nil {
		t.Fatalf("unexpected error starting layer upload: %s", err)
	}
	contents := []byte{1, 2, 3}
	blobUpload.Write(contents)
	blobUpload.Close()
	offset := blobUpload.Size()
	if offset != int64(len(contents)) {
		t.Fatalf("unexpected value for blobUpload offset:  %v != %v", offset, len(contents))
	}
}

// TestSimpleBlobUpload covers the blob upload process, exercising common
// error paths that might be seen during an upload.
func TestSimpleBlobUpload(t *testing.T) {
	randomDataReader, dgst, err := testutil.CreateRandomTarFile()
	if err != nil {
		t.Fatalf("error creating random reader: %v", err)
	}

	ctx := context.Background()
	imageName, _ := reference.WithName("foo/bar")
	driver := testdriver.New()
	registry, err := NewRegistry(ctx, driver, BlobDescriptorCacheProvider(memory.NewInMemoryBlobDescriptorCacheProvider(memory.UnlimitedSize)), EnableDelete, EnableRedirect)
	if err != nil {
		t.Fatalf("error creating registry: %v", err)
	}
	repository, err := registry.Repository(ctx, imageName)
	if err != nil {
		t.Fatalf("unexpected error getting repo: %v", err)
	}
	bs := repository.Blobs(ctx)

	h := sha256.New()
	rd := io.TeeReader(randomDataReader, h)

	blobUpload, err := bs.Create(ctx)
	if err != nil {
		t.Fatalf("unexpected error starting layer upload: %s", err)
	}

	// Cancel the upload then restart it
	if err := blobUpload.Cancel(ctx); err != nil {
		t.Fatalf("unexpected error during upload cancellation: %v", err)
	}

	// get the enclosing directory
	uploadPath := path.Dir(blobUpload.(*blobWriter).path)

	// ensure state was cleaned up
	_, err = driver.List(ctx, uploadPath)
	if err == nil {
		t.Fatal("files in upload path after cleanup")
	}

	// Do a resume, get unknown upload
	_, err = bs.Resume(ctx, blobUpload.ID())
	if err != distribution.ErrBlobUploadUnknown {
		t.Fatalf("unexpected error resuming upload, should be unknown: %v", err)
	}

	// Restart!
	blobUpload, err = bs.Create(ctx)
	if err != nil {
		t.Fatalf("unexpected error starting layer upload: %s", err)
	}

	// Get the size of our random tarfile
	randomDataSize, err := seekerSize(randomDataReader)
	if err != nil {
		t.Fatalf("error getting seeker size of random data: %v", err)
	}

	nn, err := io.Copy(blobUpload, rd)
	if err != nil {
		t.Fatalf("unexpected error uploading layer data: %v", err)
	}

	if nn != randomDataSize {
		t.Fatalf("layer data write incomplete")
	}

	blobUpload.Close()

	offset := blobUpload.Size()
	if offset != nn {
		t.Fatalf("blobUpload not updated with correct offset: %v != %v", offset, nn)
	}

	// Do a resume, for good fun
	blobUpload, err = bs.Resume(ctx, blobUpload.ID())
	if err != nil {
		t.Fatalf("unexpected error resuming upload: %v", err)
	}

	sha256Digest := digest.NewDigest("sha256", h)
	desc, err := blobUpload.Commit(ctx, distribution.Descriptor{Digest: dgst})
	if err != nil {
		t.Fatalf("unexpected error finishing layer upload: %v", err)
	}

	// ensure state was cleaned up
	uploadPath = path.Dir(blobUpload.(*blobWriter).path)
	_, err = driver.List(ctx, uploadPath)
	if err == nil {
		t.Fatal("files in upload path after commit")
	}

	// After finishing an upload, it should no longer exist.
	if _, err := bs.Resume(ctx, blobUpload.ID()); err != distribution.ErrBlobUploadUnknown {
		t.Fatalf("expected layer upload to be unknown, got %v", err)
	}

	// Test for existence.
	statDesc, err := bs.Stat(ctx, desc.Digest)
	if err != nil {
		t.Fatalf("unexpected error checking for existence: %v, %#v", err, bs)
	}

	if !reflect.DeepEqual(statDesc, desc) {
		t.Fatalf("descriptors not equal: %v != %v", statDesc, desc)
	}

	rc, err := bs.Open(ctx, desc.Digest)
	if err != nil {
		t.Fatalf("unexpected error opening blob for read: %v", err)
	}
	defer rc.Close()

	h.Reset()
	nn, err = io.Copy(h, rc)
	if err != nil {
		t.Fatalf("error reading layer: %v", err)
	}

	if nn != randomDataSize {
		t.Fatalf("incorrect read length")
	}

	if digest.NewDigest("sha256", h) != sha256Digest {
		t.Fatalf("unexpected digest from uploaded layer: %q != %q", digest.NewDigest("sha256", h), sha256Digest)
	}

	// Delete a blob
	err = bs.Delete(ctx, desc.Digest)
	if err != nil {
		t.Fatalf("Unexpected error deleting blob")
	}

	d, err := bs.Stat(ctx, desc.Digest)
	if err == nil {
		t.Fatalf("unexpected non-error stating deleted blob: %v", d)
	}

	switch err {
	case distribution.ErrBlobUnknown:
		break
	default:
		t.Errorf("Unexpected error type stat-ing deleted manifest: %#v", err)
	}

	_, err = bs.Open(ctx, desc.Digest)
	if err == nil {
		t.Fatalf("unexpected success opening deleted blob for read")
	}

	switch err {
	case distribution.ErrBlobUnknown:
		break
	default:
		t.Errorf("Unexpected error type getting deleted manifest: %#v", err)
	}

	// Re-upload the blob
	randomBlob, err := io.ReadAll(randomDataReader)
	if err != nil {
		t.Fatalf("Error reading all of blob %s", err.Error())
	}
	expectedDigest := digest.FromBytes(randomBlob)
	simpleUpload(t, bs, randomBlob, expectedDigest)

	d, err = bs.Stat(ctx, expectedDigest)
	if err != nil {
		t.Errorf("unexpected error stat-ing blob")
	}
	if d.Digest != expectedDigest {
		t.Errorf("Mismatching digest with restored blob")
	}

	_, err = bs.Open(ctx, expectedDigest)
	if err != nil {
		t.Errorf("Unexpected error opening blob")
	}

	// Reuse state to test delete with a delete-disabled registry
	registry, err = NewRegistry(ctx, driver, BlobDescriptorCacheProvider(memory.NewInMemoryBlobDescriptorCacheProvider(memory.UnlimitedSize)), EnableRedirect)
	if err != nil {
		t.Fatalf("error creating registry: %v", err)
	}
	repository, err = registry.Repository(ctx, imageName)
	if err != nil {
		t.Fatalf("unexpected error getting repo: %v", err)
	}
	bs = repository.Blobs(ctx)
	err = bs.Delete(ctx, desc.Digest)
	if err == nil {
		t.Errorf("Unexpected success deleting while disabled")
	}
}

// TestSimpleBlobRead just creates a simple blob file and ensures that basic
// open, read, seek, read works. More specific edge cases should be covered in
// other tests.
func TestSimpleBlobRead(t *testing.T) {
	ctx := context.Background()
	imageName, _ := reference.WithName("foo/bar")
	driver := testdriver.New()
	registry, err := NewRegistry(ctx, driver, BlobDescriptorCacheProvider(memory.NewInMemoryBlobDescriptorCacheProvider(memory.UnlimitedSize)), EnableDelete, EnableRedirect)
	if err != nil {
		t.Fatalf("error creating registry: %v", err)
	}
	repository, err := registry.Repository(ctx, imageName)
	if err != nil {
		t.Fatalf("unexpected error getting repo: %v", err)
	}
	bs := repository.Blobs(ctx)

	randomLayerReader, dgst, err := testutil.CreateRandomTarFile() // TODO(stevvooe): Consider using just a random string.
	if err != nil {
		t.Fatalf("error creating random data: %v", err)
	}

	// Test for existence.
	desc, err := bs.Stat(ctx, dgst)
	if err != distribution.ErrBlobUnknown {
		t.Fatalf("expected not found error when testing for existence: %v", err)
	}

	_, err = bs.Open(ctx, dgst)
	if err != distribution.ErrBlobUnknown {
		t.Fatalf("expected not found error when opening non-existent blob: %v", err)
	}

	randomLayerSize, err := seekerSize(randomLayerReader)
	if err != nil {
		t.Fatalf("error getting seeker size for random layer: %v", err)
	}

	descBefore := distribution.Descriptor{Digest: dgst, MediaType: "application/octet-stream", Size: randomLayerSize}
	t.Logf("desc: %v", descBefore)

	desc, err = addBlob(ctx, bs, descBefore, randomLayerReader)
	if err != nil {
		t.Fatalf("error adding blob to blobservice: %v", err)
	}

	if desc.Size != randomLayerSize {
		t.Fatalf("committed blob has incorrect length: %v != %v", desc.Size, randomLayerSize)
	}

	rc, err := bs.Open(ctx, desc.Digest) // note that we are opening with original digest.
	if err != nil {
		t.Fatalf("error opening blob with %v: %v", dgst, err)
	}
	defer rc.Close()

	// Now check the sha digest and ensure its the same
	h := sha256.New()
	nn, err := io.Copy(h, rc)
	if err != nil {
		t.Fatalf("unexpected error copying to hash: %v", err)
	}

	if nn != randomLayerSize {
		t.Fatalf("stored incorrect number of bytes in blob: %d != %d", nn, randomLayerSize)
	}

	sha256Digest := digest.NewDigest("sha256", h)
	if sha256Digest != desc.Digest {
		t.Fatalf("fetched digest does not match: %q != %q", sha256Digest, desc.Digest)
	}

	// Now seek back the blob, read the whole thing and check against randomLayerData
	offset, err := rc.Seek(0, io.SeekStart)
	if err != nil {
		t.Fatalf("error seeking blob: %v", err)
	}

	if offset != 0 {
		t.Fatalf("seek failed: expected 0 offset, got %d", offset)
	}

	p, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("error reading all of blob: %v", err)
	}

	if len(p) != int(randomLayerSize) {
		t.Fatalf("blob data read has different length: %v != %v", len(p), randomLayerSize)
	}

	// Reset the randomLayerReader and read back the buffer
	_, err = randomLayerReader.Seek(0, io.SeekStart)
	if err != nil {
		t.Fatalf("error resetting layer reader: %v", err)
	}

	randomLayerData, err := io.ReadAll(randomLayerReader)
	if err != nil {
		t.Fatalf("random layer read failed: %v", err)
	}

	if !bytes.Equal(p, randomLayerData) {
		t.Fatalf("layer data not equal")
	}
}

type source struct {
	digest     digest.Digest
	imageName  reference.Named
	repository distribution.Repository
	blobs      distribution.BlobStore
	desc       distribution.Descriptor
}

func createSource(ctx context.Context, t *testing.T, registry distribution.Namespace) *source {
	reader, digest, err := testutil.CreateRandomTarFile()
	if err != nil {
		t.Fatalf("error creating random reader: %v", err)
	}

	imageName, _ := reference.WithName(fmt.Sprintf("foo/source-%d", rand.Int()))

	repository, err := registry.Repository(ctx, imageName)
	if err != nil {
		t.Fatalf("unexpected error getting repo: %v", err)
	}

	blobs := repository.Blobs(ctx)

	upload, err := blobs.Create(ctx)
	if err != nil {
		t.Fatalf("unexpected error starting layer upload: %s", err)
	}

	// Get the size of our random tarfile
	randomDataSize, err := seekerSize(reader)
	if err != nil {
		t.Fatalf("error getting seeker size of random data: %v", err)
	}

	_, err = io.Copy(upload, reader)
	if err != nil {
		t.Fatalf("unexpected error uploading layer data: %v", err)
	}

	desc, err := upload.Commit(ctx, distribution.Descriptor{Digest: digest})
	if err != nil {
		t.Fatalf("unexpected error finishing layer upload: %v", err)
	}

	if desc.Size != randomDataSize {
		t.Fatalf("Committed data size %d does not match uploaded expected data size %d", desc.Size, randomDataSize)
	}

	if desc.Digest != digest {
		t.Fatalf("Committed digest %s does not match expected digest %s", desc.Digest, digest)
	}

	// Test for existence.
	statDesc, err := blobs.Stat(ctx, desc.Digest)
	if err != nil {
		t.Fatalf("unexpected error checking for existence: %v, %#v", err, blobs)
	}

	if !reflect.DeepEqual(statDesc, desc) {
		t.Fatalf("descriptors not equal: %v != %v", statDesc, desc)
	}

	return &source{
		digest:     digest,
		imageName:  imageName,
		repository: repository,
		blobs:      blobs,
		desc:       desc,
	}
}

func TestBlobMountAutomaticContentDiscovery(t *testing.T) {
	t.Run("testBlobMountAutomaticContent", func(t *testing.T) {
		testBlobMountAutomaticContentDiscovery(t, false)
	})
	t.Run("testBlobMountAutomaticContentDiscoveryEnabled", func(t *testing.T) {
		testBlobMountAutomaticContentDiscovery(t, true)
	})
}

// testBlobMountAutomaticContentDiscovery covers the blob mount process, exercising common
// error paths that might be seen during a mount.
func testBlobMountAutomaticContentDiscovery(t *testing.T, enableAutomaticContentDiscovery bool) {
	ctx := context.Background()

	driver := testdriver.New()
	var registry distribution.Namespace
	var err error
	if enableAutomaticContentDiscovery {
		registry, err = NewRegistry(ctx, driver, BlobDescriptorCacheProvider(memory.NewInMemoryBlobDescriptorCacheProvider()), EnableDelete, EnableRedirect, EnableAutomaticContentDiscovery)
	} else {
		registry, err = NewRegistry(ctx, driver, BlobDescriptorCacheProvider(memory.NewInMemoryBlobDescriptorCacheProvider()), EnableDelete, EnableRedirect)
	}
	if err != nil {
		t.Fatalf("error creating registry: %v", err)
	}

	source := createSource(ctx, t, registry)
	source2 := createSource(ctx, t, registry)
	canonicalRef, err := reference.WithDigest(source.repository.Named(), source.desc.Digest)
	if err != nil {
		t.Fatal(err)
	}

	imageName, _ := reference.WithName("foo/bar")
	repository, err := registry.Repository(ctx, imageName)
	if err != nil {
		t.Fatalf("unexpected error getting repo: %v", err)
	}
	bs := repository.Blobs(ctx)

	var bw distribution.BlobWriter
	if enableAutomaticContentDiscovery {
		bw, err = bs.Create(ctx, WithMount(source.digest))
	} else {
		// Make sure without automatic content discovery this fails gracefully
		bw, err = bs.Create(ctx, WithMount(source.digest))
		if err != nil {
			t.Fatalf("Received unexpected non-error, when cross-mounting without canonical reference and automatic content discovery disabled")
		}
		if bw == nil {
			t.Fatalf("Did not start upload upon failed cross mount with automatic content discovery disabled")
		}

		bw, err = bs.Create(ctx, WithMountFrom(canonicalRef))
	}
	if bw != nil {
		t.Fatal("unexpected blobwriter returned from Create call, should mount instead")
	}

	var mountedDescriptor distribution.Descriptor
	if enableAutomaticContentDiscovery {
		ebm := distribution.ErrBlobMounted{}
		if !errors.As(err, &ebm) {
			t.Fatalf("unexpected error mounting layer: %v", err)
		}
		mountedDescriptor = ebm.Descriptor
	} else {
		ebmf := distribution.ErrBlobMountedFrom{}
		if !errors.As(err, &ebmf) {
			t.Fatalf("unexpected error mounting layer: %v", err)
		}
		mountedDescriptor = ebmf.Descriptor
	}

	if !reflect.DeepEqual(mountedDescriptor, source.desc) {
		t.Fatalf("descriptors not equal: %v != %v", mountedDescriptor, source.desc)
	}

	// Test the negative case
	// This uses the source repo from the first repo, but the digest from the second
	// Even in automatic content discovery, it is stated that the from *MAY* be ignored, but
	// is not required to be ignored.
	wrongCanonicalRef, err := reference.WithDigest(source.repository.Named(), source2.desc.Digest)
	if err != nil {
		t.Fatal(err)
	}
	bw2, err := bs.Create(ctx, WithMountFrom(wrongCanonicalRef))
	if err != nil {
		t.Fatalf("Received unexpected non-error, when uploading with cross-mount from \"wrong\" repository")
	}
	if bw2 == nil {
		t.Fatalf("Did not start upload upon failed cross mount")
	}

	// Test for existence.
	statDesc, err := bs.Stat(ctx, source.desc.Digest)
	if err != nil {
		t.Fatalf("unexpected error checking for existence: %v, %#v", err, bs)
	}

	if !reflect.DeepEqual(statDesc, source.desc) {
		t.Fatalf("descriptors not equal: %v != %v", statDesc, source.desc)
	}

	rc, err := bs.Open(ctx, source.digest)
	if err != nil {
		t.Fatalf("unexpected error opening blob for read: %v", err)
	}
	defer rc.Close()

	h := sha256.New()
	nn, err := io.Copy(h, rc)
	if err != nil {
		t.Fatalf("error reading layer: %v", err)
	}

	if nn != source.desc.Size {
		t.Fatalf("incorrect read length")
	}

	if digest.NewDigest("sha256", h) != source.digest {
		t.Fatalf("unexpected digest from uploaded layer: %q != %q", digest.NewDigest("sha256", h), source.digest)
	}

	// Delete the blob from the source repo
	err = source.blobs.Delete(ctx, source.digest)
	if err != nil {
		t.Fatalf("Unexpected error deleting blob")
	}

	_, err = bs.Stat(ctx, source.digest)
	if err != nil {
		t.Fatalf("unexpected error stating blob deleted from source repository: %v", err)
	}

	d, err := source.blobs.Stat(ctx, source.desc.Digest)
	if err == nil {
		t.Fatalf("unexpected non-error stating deleted blob: %v", d)
	}

	switch err {
	case distribution.ErrBlobUnknown:
		break
	default:
		t.Errorf("Unexpected error type stat-ing deleted manifest: %#v", err)
	}

	// Delete the blob from the dest repo
	err = bs.Delete(ctx, source.digest)
	if err != nil {
		t.Fatalf("Unexpected error deleting blob")
	}

	d, err = bs.Stat(ctx, source.digest)
	if err == nil {
		t.Fatalf("unexpected non-error stating deleted blob: %v", d)
	}

	switch err {
	case distribution.ErrBlobUnknown:
		break
	default:
		t.Errorf("Unexpected error type stat-ing deleted manifest: %#v", err)
	}
}

// TestLayerUploadZeroLength uploads zero-length
func TestLayerUploadZeroLength(t *testing.T) {
	ctx := context.Background()
	imageName, _ := reference.WithName("foo/bar")
	driver := testdriver.New()
	registry, err := NewRegistry(ctx, driver, BlobDescriptorCacheProvider(memory.NewInMemoryBlobDescriptorCacheProvider(memory.UnlimitedSize)), EnableDelete, EnableRedirect)
	if err != nil {
		t.Fatalf("error creating registry: %v", err)
	}
	repository, err := registry.Repository(ctx, imageName)
	if err != nil {
		t.Fatalf("unexpected error getting repo: %v", err)
	}
	bs := repository.Blobs(ctx)

	simpleUpload(t, bs, []byte{}, digestSha256Empty)
}

func simpleUpload(t *testing.T, bs distribution.BlobIngester, blob []byte, expectedDigest digest.Digest) {
	ctx := context.Background()
	wr, err := bs.Create(ctx)
	if err != nil {
		t.Fatalf("unexpected error starting upload: %v", err)
	}

	nn, err := io.Copy(wr, bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("error copying into blob writer: %v", err)
	}

	if nn != 0 {
		t.Fatalf("unexpected number of bytes copied: %v > 0", nn)
	}

	dgst, err := digest.FromReader(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("error getting digest: %v", err)
	}

	if dgst != expectedDigest {
		// sanity check on zero digest
		t.Fatalf("digest not as expected: %v != %v", dgst, expectedDigest)
	}

	desc, err := wr.Commit(ctx, distribution.Descriptor{Digest: dgst})
	if err != nil {
		t.Fatalf("unexpected error committing write: %v", err)
	}

	if desc.Digest != dgst {
		t.Fatalf("unexpected digest: %v != %v", desc.Digest, dgst)
	}
}

// seekerSize seeks to the end of seeker, checks the size and returns it to
// the original state, returning the size. The state of the seeker should be
// treated as unknown if an error is returned.
func seekerSize(seeker io.ReadSeeker) (int64, error) {
	current, err := seeker.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}

	end, err := seeker.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}

	resumed, err := seeker.Seek(current, io.SeekStart)
	if err != nil {
		return 0, err
	}

	if resumed != current {
		return 0, fmt.Errorf("error returning seeker to original state, could not seek back to original location")
	}

	return end, nil
}

// addBlob simply consumes the reader and inserts into the blob service,
// returning a descriptor on success.
func addBlob(ctx context.Context, bs distribution.BlobIngester, desc distribution.Descriptor, rd io.Reader) (distribution.Descriptor, error) {
	wr, err := bs.Create(ctx)
	if err != nil {
		return distribution.Descriptor{}, err
	}
	defer wr.Cancel(ctx)

	if nn, err := io.Copy(wr, rd); err != nil {
		return distribution.Descriptor{}, err
	} else if nn != desc.Size {
		return distribution.Descriptor{}, fmt.Errorf("incorrect number of bytes copied: %v != %v", nn, desc.Size)
	}

	return wr.Commit(ctx, desc)
}
