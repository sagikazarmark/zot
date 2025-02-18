package imagestore

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/docker/distribution/registry/storage/driver"
	guuid "github.com/gofrs/uuid"
	godigest "github.com/opencontainers/go-digest"
	ispec "github.com/opencontainers/image-spec/specs-go/v1"
	artifactspec "github.com/oras-project/artifacts-spec/specs-go/v1"

	zerr "zotregistry.io/zot/errors"
	zcommon "zotregistry.io/zot/pkg/common"
	"zotregistry.io/zot/pkg/extensions/monitoring"
	syncConstants "zotregistry.io/zot/pkg/extensions/sync/constants"
	zlog "zotregistry.io/zot/pkg/log"
	zreg "zotregistry.io/zot/pkg/regexp"
	"zotregistry.io/zot/pkg/scheduler"
	"zotregistry.io/zot/pkg/storage/cache"
	common "zotregistry.io/zot/pkg/storage/common"
	storageConstants "zotregistry.io/zot/pkg/storage/constants"
	storageTypes "zotregistry.io/zot/pkg/storage/types"
	"zotregistry.io/zot/pkg/test/inject"
)

const (
	cosignSignatureTagSuffix = "sig"
	SBOMTagSuffix            = "sbom"
)

// ImageStore provides the image storage operations.
type ImageStore struct {
	rootDir        string
	storeDriver    storageTypes.Driver
	lock           *sync.RWMutex
	log            zlog.Logger
	metrics        monitoring.MetricServer
	cache          cache.Cache
	dedupe         bool
	linter         common.Lint
	commit         bool
	gc             bool
	gcReferrers    bool
	gcDelay        time.Duration
	retentionDelay time.Duration
}

func (is *ImageStore) RootDir() string {
	return is.rootDir
}

func (is *ImageStore) DirExists(d string) bool {
	return is.storeDriver.DirExists(d)
}

// NewImageStore returns a new image store backed by cloud storages.
// see https://github.com/docker/docker.github.io/tree/master/registry/storage-drivers
// Use the last argument to properly set a cache database, or it will default to boltDB local storage.
func NewImageStore(rootDir string, cacheDir string, gc bool, gcReferrers bool, gcDelay time.Duration,
	untaggedImageRetentionDelay time.Duration, dedupe, commit bool, log zlog.Logger, metrics monitoring.MetricServer,
	linter common.Lint, storeDriver storageTypes.Driver, cacheDriver cache.Cache,
) storageTypes.ImageStore {
	if err := storeDriver.EnsureDir(rootDir); err != nil {
		log.Error().Err(err).Str("rootDir", rootDir).Msg("unable to create root dir")

		return nil
	}

	imgStore := &ImageStore{
		rootDir:        rootDir,
		storeDriver:    storeDriver,
		lock:           &sync.RWMutex{},
		log:            log,
		metrics:        metrics,
		dedupe:         dedupe,
		linter:         linter,
		commit:         commit,
		gc:             gc,
		gcReferrers:    gcReferrers,
		gcDelay:        gcDelay,
		retentionDelay: untaggedImageRetentionDelay,
		cache:          cacheDriver,
	}

	return imgStore
}

// RLock read-lock.
func (is *ImageStore) RLock(lockStart *time.Time) {
	*lockStart = time.Now()

	is.lock.RLock()
}

// RUnlock read-unlock.
func (is *ImageStore) RUnlock(lockStart *time.Time) {
	is.lock.RUnlock()

	lockEnd := time.Now()
	// includes time spent in acquiring and holding a lock
	latency := lockEnd.Sub(*lockStart)
	monitoring.ObserveStorageLockLatency(is.metrics, latency, is.RootDir(), storageConstants.RLOCK) // histogram
}

// Lock write-lock.
func (is *ImageStore) Lock(lockStart *time.Time) {
	*lockStart = time.Now()

	is.lock.Lock()
}

// Unlock write-unlock.
func (is *ImageStore) Unlock(lockStart *time.Time) {
	is.lock.Unlock()

	lockEnd := time.Now()
	// includes time spent in acquiring and holding a lock
	latency := lockEnd.Sub(*lockStart)
	monitoring.ObserveStorageLockLatency(is.metrics, latency, is.RootDir(), storageConstants.RWLOCK) // histogram
}

func (is *ImageStore) initRepo(name string) error {
	repoDir := path.Join(is.rootDir, name)

	if !utf8.ValidString(name) {
		is.log.Error().Msg("input is not valid UTF-8")

		return zerr.ErrInvalidRepositoryName
	}

	if !zreg.FullNameRegexp.MatchString(name) {
		is.log.Error().Str("repository", name).Msg("invalid repository name")

		return zerr.ErrInvalidRepositoryName
	}

	// create "blobs" subdir
	err := is.storeDriver.EnsureDir(path.Join(repoDir, "blobs"))
	if err != nil {
		is.log.Error().Err(err).Msg("error creating blobs subdir")

		return err
	}
	// create BlobUploadDir subdir
	err = is.storeDriver.EnsureDir(path.Join(repoDir, storageConstants.BlobUploadDir))
	if err != nil {
		is.log.Error().Err(err).Msg("error creating blob upload subdir")

		return err
	}

	// "oci-layout" file - create if it doesn't exist
	ilPath := path.Join(repoDir, ispec.ImageLayoutFile)
	if _, err := is.storeDriver.Stat(ilPath); err != nil {
		il := ispec.ImageLayout{Version: ispec.ImageLayoutVersion}

		buf, err := json.Marshal(il)
		if err != nil {
			is.log.Error().Err(err).Msg("unable to marshal JSON")

			return err
		}

		if _, err := is.storeDriver.WriteFile(ilPath, buf); err != nil {
			is.log.Error().Err(err).Str("file", ilPath).Msg("unable to write file")

			return err
		}
	}

	// "index.json" file - create if it doesn't exist
	indexPath := path.Join(repoDir, "index.json")
	if _, err := is.storeDriver.Stat(indexPath); err != nil {
		index := ispec.Index{}
		index.SchemaVersion = 2

		buf, err := json.Marshal(index)
		if err != nil {
			is.log.Error().Err(err).Msg("unable to marshal JSON")

			return err
		}

		if _, err := is.storeDriver.WriteFile(indexPath, buf); err != nil {
			is.log.Error().Err(err).Str("file", ilPath).Msg("unable to write file")

			return err
		}
	}

	return nil
}

// InitRepo creates an image repository under this store.
func (is *ImageStore) InitRepo(name string) error {
	var lockLatency time.Time

	is.Lock(&lockLatency)
	defer is.Unlock(&lockLatency)

	return is.initRepo(name)
}

// ValidateRepo validates that the repository layout is complaint with the OCI repo layout.
func (is *ImageStore) ValidateRepo(name string) (bool, error) {
	if !zreg.FullNameRegexp.MatchString(name) {
		return false, zerr.ErrInvalidRepositoryName
	}

	// https://github.com/opencontainers/image-spec/blob/master/image-layout.md#content
	// at least, expect at least 3 entries - ["blobs", "oci-layout", "index.json"]
	// and an additional/optional BlobUploadDir in each image store
	// for s3 we can not create empty dirs, so we check only against index.json and oci-layout
	dir := path.Join(is.rootDir, name)
	if fi, err := is.storeDriver.Stat(dir); err != nil || !fi.IsDir() {
		return false, zerr.ErrRepoNotFound
	}

	files, err := is.storeDriver.List(dir)
	if err != nil {
		is.log.Error().Err(err).Str("dir", dir).Msg("unable to read directory")

		return false, zerr.ErrRepoNotFound
	}

	//nolint:gomnd
	if len(files) < 2 {
		return false, zerr.ErrRepoBadVersion
	}

	found := map[string]bool{
		ispec.ImageLayoutFile: false,
		"index.json":          false,
	}

	for _, file := range files {
		fileInfo, err := is.storeDriver.Stat(file)
		if err != nil {
			return false, err
		}

		filename, err := filepath.Rel(dir, file)
		if err != nil {
			return false, err
		}

		if filename == "blobs" && !fileInfo.IsDir() {
			return false, nil
		}

		found[filename] = true
	}

	// check blobs dir exists only for filesystem, in s3 we can't have empty dirs
	if is.storeDriver.Name() == storageConstants.LocalStorageDriverName {
		if !is.storeDriver.DirExists(path.Join(dir, "blobs")) {
			return false, nil
		}
	}

	for k, v := range found {
		if !v && k != storageConstants.BlobUploadDir {
			return false, nil
		}
	}

	buf, err := is.storeDriver.ReadFile(path.Join(dir, ispec.ImageLayoutFile))
	if err != nil {
		return false, err
	}

	var il ispec.ImageLayout
	if err := json.Unmarshal(buf, &il); err != nil {
		return false, err
	}

	if il.Version != ispec.ImageLayoutVersion {
		return false, zerr.ErrRepoBadVersion
	}

	return true, nil
}

// GetRepositories returns a list of all the repositories under this store.
func (is *ImageStore) GetRepositories() ([]string, error) {
	var lockLatency time.Time

	dir := is.rootDir

	is.RLock(&lockLatency)
	defer is.RUnlock(&lockLatency)

	stores := make([]string, 0)

	err := is.storeDriver.Walk(dir, func(fileInfo driver.FileInfo) error {
		if !fileInfo.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(is.rootDir, fileInfo.Path())
		if err != nil {
			return nil //nolint:nilerr // ignore paths that are not under root dir
		}

		if ok, err := is.ValidateRepo(rel); !ok || err != nil {
			return nil //nolint:nilerr // ignore invalid repos
		}

		stores = append(stores, rel)

		return nil
	})

	// if the root directory is not yet created then return an empty slice of repositories
	var perr driver.PathNotFoundError
	if errors.As(err, &perr) {
		return stores, nil
	}

	return stores, err
}

// GetNextRepository returns next repository under this store.
func (is *ImageStore) GetNextRepository(repo string) (string, error) {
	var lockLatency time.Time

	dir := is.rootDir

	is.RLock(&lockLatency)
	defer is.RUnlock(&lockLatency)

	_, err := is.storeDriver.List(dir)
	if err != nil {
		is.log.Error().Err(err).Msg("failure walking storage root-dir")

		return "", err
	}

	found := false
	store := ""
	err = is.storeDriver.Walk(dir, func(fileInfo driver.FileInfo) error {
		if !fileInfo.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(is.rootDir, fileInfo.Path())
		if err != nil {
			return nil //nolint:nilerr // ignore paths not relative to root dir
		}

		ok, err := is.ValidateRepo(rel)
		if !ok || err != nil {
			return nil //nolint:nilerr // ignore invalid repos
		}

		if repo == "" && ok && err == nil {
			store = rel

			return io.EOF
		}

		if found {
			store = rel

			return io.EOF
		}

		if rel == repo {
			found = true
		}

		return nil
	})

	driverErr := &driver.Error{}

	if errors.Is(err, io.EOF) ||
		(errors.As(err, driverErr) && errors.Is(driverErr.Enclosed, io.EOF)) {
		return store, nil
	}

	return store, err
}

// GetImageTags returns a list of image tags available in the specified repository.
func (is *ImageStore) GetImageTags(repo string) ([]string, error) {
	var lockLatency time.Time

	dir := path.Join(is.rootDir, repo)
	if fi, err := is.storeDriver.Stat(dir); err != nil || !fi.IsDir() {
		return nil, zerr.ErrRepoNotFound
	}

	is.RLock(&lockLatency)
	defer is.RUnlock(&lockLatency)

	index, err := common.GetIndex(is, repo, is.log)
	if err != nil {
		return nil, err
	}

	return common.GetTagsByIndex(index), nil
}

// GetImageManifest returns the image manifest of an image in the specific repository.
func (is *ImageStore) GetImageManifest(repo, reference string) ([]byte, godigest.Digest, string, error) {
	dir := path.Join(is.rootDir, repo)
	if fi, err := is.storeDriver.Stat(dir); err != nil || !fi.IsDir() {
		return nil, "", "", zerr.ErrRepoNotFound
	}

	var lockLatency time.Time

	var err error

	is.RLock(&lockLatency)
	defer func() {
		is.RUnlock(&lockLatency)

		if err == nil {
			monitoring.IncDownloadCounter(is.metrics, repo)
		}
	}()

	index, err := common.GetIndex(is, repo, is.log)
	if err != nil {
		return nil, "", "", err
	}

	manifestDesc, found := common.GetManifestDescByReference(index, reference)
	if !found {
		return nil, "", "", zerr.ErrManifestNotFound
	}

	buf, err := is.GetBlobContent(repo, manifestDesc.Digest)
	if err != nil {
		if errors.Is(err, zerr.ErrBlobNotFound) {
			return nil, "", "", zerr.ErrManifestNotFound
		}

		return nil, "", "", err
	}

	var manifest ispec.Manifest
	if err := json.Unmarshal(buf, &manifest); err != nil {
		is.log.Error().Err(err).Str("dir", dir).Msg("invalid JSON")

		return nil, "", "", err
	}

	return buf, manifestDesc.Digest, manifestDesc.MediaType, nil
}

// PutImageManifest adds an image manifest to the repository.
func (is *ImageStore) PutImageManifest(repo, reference, mediaType string, //nolint: gocyclo
	body []byte,
) (godigest.Digest, godigest.Digest, error) {
	if err := is.InitRepo(repo); err != nil {
		is.log.Debug().Err(err).Msg("init repo")

		return "", "", err
	}

	var lockLatency time.Time

	var err error

	is.Lock(&lockLatency)
	defer func() {
		is.Unlock(&lockLatency)

		if err == nil {
			monitoring.SetStorageUsage(is.metrics, is.rootDir, repo)
			monitoring.IncUploadCounter(is.metrics, repo)
		}
	}()

	refIsDigest := true

	mDigest, err := common.GetAndValidateRequestDigest(body, reference, is.log)
	if err != nil {
		if errors.Is(err, zerr.ErrBadManifest) {
			return mDigest, "", err
		}

		refIsDigest = false
	}

	dig, err := common.ValidateManifest(is, repo, reference, mediaType, body, is.log)
	if err != nil {
		return dig, "", err
	}

	index, err := common.GetIndex(is, repo, is.log)
	if err != nil {
		return "", "", err
	}

	// create a new descriptor
	desc := ispec.Descriptor{
		MediaType: mediaType, Size: int64(len(body)), Digest: mDigest,
	}

	if !refIsDigest {
		desc.Annotations = map[string]string{ispec.AnnotationRefName: reference}
	}

	var subjectDigest godigest.Digest

	artifactType := ""

	if mediaType == ispec.MediaTypeImageManifest {
		var manifest ispec.Manifest

		err := json.Unmarshal(body, &manifest)
		if err != nil {
			return "", "", err
		}

		if manifest.Subject != nil {
			subjectDigest = manifest.Subject.Digest
		}

		artifactType = zcommon.GetManifestArtifactType(manifest)
	} else if mediaType == ispec.MediaTypeImageIndex {
		var index ispec.Index

		err := json.Unmarshal(body, &index)
		if err != nil {
			return "", "", err
		}

		if index.Subject != nil {
			subjectDigest = index.Subject.Digest
		}

		artifactType = zcommon.GetIndexArtifactType(index)
	}

	updateIndex, oldDgst, err := common.CheckIfIndexNeedsUpdate(&index, &desc, is.log)
	if err != nil {
		return "", "", err
	}

	if !updateIndex {
		return desc.Digest, subjectDigest, nil
	}

	// write manifest to "blobs"
	dir := path.Join(is.rootDir, repo, "blobs", mDigest.Algorithm().String())
	manifestPath := path.Join(dir, mDigest.Encoded())

	if _, err = is.storeDriver.WriteFile(manifestPath, body); err != nil {
		is.log.Error().Err(err).Str("file", manifestPath).Msg("unable to write")

		return "", "", err
	}

	err = common.UpdateIndexWithPrunedImageManifests(is, &index, repo, desc, oldDgst, is.log)
	if err != nil {
		return "", "", err
	}

	// now update "index.json"
	index.Manifests = append(index.Manifests, desc)
	dir = path.Join(is.rootDir, repo)
	indexPath := path.Join(dir, "index.json")

	buf, err := json.Marshal(index)
	if err != nil {
		is.log.Error().Err(err).Str("file", indexPath).Msg("unable to marshal JSON")

		return "", "", err
	}

	// update the descriptors artifact type in order to check for signatures when applying the linter
	desc.ArtifactType = artifactType

	// apply linter only on images, not signatures
	pass, err := common.ApplyLinter(is, is.linter, repo, desc)
	if !pass {
		is.log.Error().Err(err).Str("repository", repo).Str("reference", reference).Msg("linter didn't pass")

		return "", "", err
	}

	if _, err = is.storeDriver.WriteFile(indexPath, buf); err != nil {
		is.log.Error().Err(err).Str("file", manifestPath).Msg("unable to write")

		return "", "", err
	}

	return desc.Digest, subjectDigest, nil
}

// DeleteImageManifest deletes the image manifest from the repository.
func (is *ImageStore) DeleteImageManifest(repo, reference string, detectCollisions bool) error {
	dir := path.Join(is.rootDir, repo)
	if fi, err := is.storeDriver.Stat(dir); err != nil || !fi.IsDir() {
		return zerr.ErrRepoNotFound
	}

	var lockLatency time.Time

	var err error

	is.Lock(&lockLatency)
	defer func() {
		is.Unlock(&lockLatency)

		if err == nil {
			monitoring.SetStorageUsage(is.metrics, is.rootDir, repo)
		}
	}()

	err = is.deleteImageManifest(repo, reference, detectCollisions)
	if err != nil {
		return err
	}

	return nil
}

func (is *ImageStore) deleteImageManifest(repo, reference string, detectCollisions bool) error {
	index, err := common.GetIndex(is, repo, is.log)
	if err != nil {
		return err
	}

	manifestDesc, err := common.RemoveManifestDescByReference(&index, reference, detectCollisions)
	if err != nil {
		return err
	}

	/* check if manifest is referenced in image indexes, do not allow index images manipulations
	(ie. remove manifest being part of an image index)	*/
	if manifestDesc.MediaType == ispec.MediaTypeImageManifest {
		for _, mDesc := range index.Manifests {
			if mDesc.MediaType == ispec.MediaTypeImageIndex {
				if ok, _ := common.IsBlobReferencedInImageIndex(is, repo, manifestDesc.Digest, ispec.Index{
					Manifests: []ispec.Descriptor{mDesc},
				}, is.log); ok {
					return zerr.ErrManifestReferenced
				}
			}
		}
	}

	err = common.UpdateIndexWithPrunedImageManifests(is, &index, repo, manifestDesc, manifestDesc.Digest, is.log)
	if err != nil {
		return err
	}

	// now update "index.json"
	dir := path.Join(is.rootDir, repo)
	file := path.Join(dir, "index.json")

	buf, err := json.Marshal(index)
	if err != nil {
		return err
	}

	if _, err := is.storeDriver.WriteFile(file, buf); err != nil {
		is.log.Debug().Str("deleting reference", reference).Msg("")

		return err
	}

	// Delete blob only when blob digest not present in manifest entry.
	// e.g. 1.0.1 & 1.0.2 have same blob digest so if we delete 1.0.1, blob should not be removed.
	toDelete := true

	for _, manifest := range index.Manifests {
		if manifestDesc.Digest.String() == manifest.Digest.String() {
			toDelete = false

			break
		}
	}

	if toDelete {
		p := path.Join(dir, "blobs", manifestDesc.Digest.Algorithm().String(), manifestDesc.Digest.Encoded())

		err = is.storeDriver.Delete(p)
		if err != nil {
			return err
		}
	}

	return nil
}

// BlobUploadPath returns the upload path for a blob in this store.
func (is *ImageStore) BlobUploadPath(repo, uuid string) string {
	dir := path.Join(is.rootDir, repo)
	blobUploadPath := path.Join(dir, storageConstants.BlobUploadDir, uuid)

	return blobUploadPath
}

// NewBlobUpload returns the unique ID for an upload in progress.
func (is *ImageStore) NewBlobUpload(repo string) (string, error) {
	if err := is.InitRepo(repo); err != nil {
		is.log.Error().Err(err).Msg("error initializing repo")

		return "", err
	}

	uuid, err := guuid.NewV4()
	if err != nil {
		return "", err
	}

	uid := uuid.String()

	blobUploadPath := is.BlobUploadPath(repo, uid)

	// create multipart upload (append false)
	writer, err := is.storeDriver.Writer(blobUploadPath, false)
	if err != nil {
		is.log.Debug().Err(err).Str("blob", blobUploadPath).Msg("failed to start multipart writer")

		return "", zerr.ErrRepoNotFound
	}

	defer writer.Close()

	return uid, nil
}

// GetBlobUpload returns the current size of a blob upload.
func (is *ImageStore) GetBlobUpload(repo, uuid string) (int64, error) {
	blobUploadPath := is.BlobUploadPath(repo, uuid)

	if !utf8.ValidString(blobUploadPath) {
		is.log.Error().Msg("input is not valid UTF-8")

		return -1, zerr.ErrInvalidRepositoryName
	}

	writer, err := is.storeDriver.Writer(blobUploadPath, true)
	if err != nil {
		if errors.As(err, &driver.PathNotFoundError{}) {
			return -1, zerr.ErrUploadNotFound
		}

		return -1, err
	}

	defer writer.Close()

	return writer.Size(), nil
}

// PutBlobChunkStreamed appends another chunk of data to the specified blob. It returns
// the number of actual bytes to the blob.
func (is *ImageStore) PutBlobChunkStreamed(repo, uuid string, body io.Reader) (int64, error) {
	if err := is.InitRepo(repo); err != nil {
		return -1, err
	}

	blobUploadPath := is.BlobUploadPath(repo, uuid)

	file, err := is.storeDriver.Writer(blobUploadPath, true)
	if err != nil {
		if errors.As(err, &driver.PathNotFoundError{}) {
			return -1, zerr.ErrUploadNotFound
		}

		is.log.Error().Err(err).Msg("failed to continue multipart upload")

		return -1, err
	}

	var n int64 //nolint: varnamelen

	defer func() {
		err = file.Close()
	}()

	n, err = io.Copy(file, body)

	return n, err
}

// PutBlobChunk writes another chunk of data to the specified blob. It returns
// the number of actual bytes to the blob.
func (is *ImageStore) PutBlobChunk(repo, uuid string, from, to int64,
	body io.Reader,
) (int64, error) {
	if err := is.InitRepo(repo); err != nil {
		return -1, err
	}

	blobUploadPath := is.BlobUploadPath(repo, uuid)

	file, err := is.storeDriver.Writer(blobUploadPath, true)
	if err != nil {
		if errors.As(err, &driver.PathNotFoundError{}) {
			return -1, zerr.ErrUploadNotFound
		}

		is.log.Error().Err(err).Msg("failed to continue multipart upload")

		return -1, err
	}

	defer file.Close()

	if from != file.Size() {
		is.log.Error().Int64("expected", from).Int64("actual", file.Size()).
			Msg("invalid range start for blob upload")

		return -1, zerr.ErrBadUploadRange
	}

	n, err := io.Copy(file, body)

	return n, err
}

// BlobUploadInfo returns the current blob size in bytes.
func (is *ImageStore) BlobUploadInfo(repo, uuid string) (int64, error) {
	blobUploadPath := is.BlobUploadPath(repo, uuid)

	writer, err := is.storeDriver.Writer(blobUploadPath, true)
	if err != nil {
		if errors.As(err, &driver.PathNotFoundError{}) {
			return -1, zerr.ErrUploadNotFound
		}

		return -1, err
	}

	defer writer.Close()

	return writer.Size(), nil
}

// FinishBlobUpload finalizes the blob upload and moves blob the repository.
func (is *ImageStore) FinishBlobUpload(repo, uuid string, body io.Reader, dstDigest godigest.Digest) error {
	if err := dstDigest.Validate(); err != nil {
		return err
	}

	src := is.BlobUploadPath(repo, uuid)

	// complete multiUploadPart
	fileWriter, err := is.storeDriver.Writer(src, true)
	if err != nil {
		is.log.Error().Err(err).Str("blob", src).Msg("failed to open blob")

		return zerr.ErrUploadNotFound
	}

	if err := fileWriter.Commit(); err != nil {
		is.log.Error().Err(err).Msg("failed to commit file")

		return err
	}

	if err := fileWriter.Close(); err != nil {
		is.log.Error().Err(err).Msg("failed to close file")

		return err
	}

	fileReader, err := is.storeDriver.Reader(src, 0)
	if err != nil {
		is.log.Error().Err(err).Str("blob", src).Msg("failed to open file")

		return zerr.ErrUploadNotFound
	}

	defer fileReader.Close()

	srcDigest, err := godigest.FromReader(fileReader)
	if err != nil {
		is.log.Error().Err(err).Str("blob", src).Msg("failed to open blob")

		return zerr.ErrBadBlobDigest
	}

	if srcDigest != dstDigest {
		is.log.Error().Str("srcDigest", srcDigest.String()).
			Str("dstDigest", dstDigest.String()).Msg("actual digest not equal to expected digest")

		return zerr.ErrBadBlobDigest
	}

	dir := path.Join(is.rootDir, repo, "blobs", dstDigest.Algorithm().String())

	err = is.storeDriver.EnsureDir(dir)
	if err != nil {
		is.log.Error().Err(err).Msg("error creating blobs/sha256 dir")

		return err
	}

	dst := is.BlobPath(repo, dstDigest)

	var lockLatency time.Time

	is.Lock(&lockLatency)
	defer is.Unlock(&lockLatency)

	if is.dedupe && fmt.Sprintf("%v", is.cache) != fmt.Sprintf("%v", nil) {
		err = is.DedupeBlob(src, dstDigest, dst)
		if err := inject.Error(err); err != nil {
			is.log.Error().Err(err).Str("src", src).Str("dstDigest", dstDigest.String()).
				Str("dst", dst).Msg("unable to dedupe blob")

			return err
		}
	} else {
		if err := is.storeDriver.Move(src, dst); err != nil {
			is.log.Error().Err(err).Str("src", src).Str("dstDigest", dstDigest.String()).
				Str("dst", dst).Msg("unable to finish blob")

			return err
		}
	}

	return nil
}

// FullBlobUpload handles a full blob upload, and no partial session is created.
func (is *ImageStore) FullBlobUpload(repo string, body io.Reader, dstDigest godigest.Digest) (string, int64, error) {
	if err := dstDigest.Validate(); err != nil {
		return "", -1, err
	}

	if err := is.InitRepo(repo); err != nil {
		return "", -1, err
	}

	u, err := guuid.NewV4()
	if err != nil {
		return "", -1, err
	}

	uuid := u.String()
	src := is.BlobUploadPath(repo, uuid)
	digester := sha256.New()
	buf := new(bytes.Buffer)

	_, err = buf.ReadFrom(body)
	if err != nil {
		is.log.Error().Err(err).Msg("failed to read blob")

		return "", -1, err
	}

	nbytes, err := is.storeDriver.WriteFile(src, buf.Bytes())
	if err != nil {
		is.log.Error().Err(err).Msg("failed to write blob")

		return "", -1, err
	}

	_, err = digester.Write(buf.Bytes())
	if err != nil {
		is.log.Error().Err(err).Msg("digester failed to write")

		return "", -1, err
	}

	srcDigest := godigest.NewDigestFromEncoded(godigest.SHA256, fmt.Sprintf("%x", digester.Sum(nil)))
	if srcDigest != dstDigest {
		is.log.Error().Str("srcDigest", srcDigest.String()).
			Str("dstDigest", dstDigest.String()).Msg("actual digest not equal to expected digest")

		return "", -1, zerr.ErrBadBlobDigest
	}

	dir := path.Join(is.rootDir, repo, "blobs", dstDigest.Algorithm().String())
	_ = is.storeDriver.EnsureDir(dir)

	var lockLatency time.Time

	is.Lock(&lockLatency)
	defer is.Unlock(&lockLatency)

	dst := is.BlobPath(repo, dstDigest)

	if is.dedupe && fmt.Sprintf("%v", is.cache) != fmt.Sprintf("%v", nil) {
		if err := is.DedupeBlob(src, dstDigest, dst); err != nil {
			is.log.Error().Err(err).Str("src", src).Str("dstDigest", dstDigest.String()).
				Str("dst", dst).Msg("unable to dedupe blob")

			return "", -1, err
		}
	} else {
		if err := is.storeDriver.Move(src, dst); err != nil {
			is.log.Error().Err(err).Str("src", src).Str("dstDigest", dstDigest.String()).
				Str("dst", dst).Msg("unable to finish blob")

			return "", -1, err
		}
	}

	return uuid, int64(nbytes), nil
}

func (is *ImageStore) DedupeBlob(src string, dstDigest godigest.Digest, dst string) error {
retry:
	is.log.Debug().Str("src", src).Str("dstDigest", dstDigest.String()).Str("dst", dst).Msg("dedupe: enter")

	dstRecord, err := is.cache.GetBlob(dstDigest)
	if err := inject.Error(err); err != nil && !errors.Is(err, zerr.ErrCacheMiss) {
		is.log.Error().Err(err).Str("blobPath", dst).Msg("dedupe: unable to lookup blob record")

		return err
	}

	if dstRecord == "" {
		// cache record doesn't exist, so first disk and cache entry for this digest
		if err := is.cache.PutBlob(dstDigest, dst); err != nil {
			is.log.Error().Err(err).Str("blobPath", dst).Msg("dedupe: unable to insert blob record")

			return err
		}

		// move the blob from uploads to final dest
		if err := is.storeDriver.Move(src, dst); err != nil {
			is.log.Error().Err(err).Str("src", src).Str("dst", dst).Msg("dedupe: unable to rename blob")

			return err
		}

		is.log.Debug().Str("src", src).Str("dst", dst).Msg("dedupe: rename")
	} else {
		// cache record exists, but due to GC and upgrades from older versions,
		// disk content and cache records may go out of sync

		if is.cache.UsesRelativePaths() {
			dstRecord = path.Join(is.rootDir, dstRecord)
		}

		_, err := is.storeDriver.Stat(dstRecord)
		if err != nil {
			is.log.Error().Err(err).Str("blobPath", dstRecord).Msg("dedupe: unable to stat")
			// the actual blob on disk may have been removed by GC, so sync the cache
			err := is.cache.DeleteBlob(dstDigest, dstRecord)
			if err = inject.Error(err); err != nil {
				//nolint:lll
				is.log.Error().Err(err).Str("dstDigest", dstDigest.String()).Str("dst", dst).Msg("dedupe: unable to delete blob record")

				return err
			}

			goto retry
		}

		// prevent overwrite original blob
		if !is.storeDriver.SameFile(dst, dstRecord) {
			if err := is.storeDriver.Link(dstRecord, dst); err != nil {
				is.log.Error().Err(err).Str("blobPath", dstRecord).Msg("dedupe: unable to link blobs")

				return err
			}

			if err := is.cache.PutBlob(dstDigest, dst); err != nil {
				is.log.Error().Err(err).Str("blobPath", dst).Msg("dedupe: unable to insert blob record")

				return err
			}
		}

		// remove temp blobupload
		if err := is.storeDriver.Delete(src); err != nil {
			is.log.Error().Err(err).Str("src", src).Msg("dedupe: unable to remove blob")

			return err
		}

		is.log.Debug().Str("src", src).Msg("dedupe: remove")
	}

	return nil
}

// DeleteBlobUpload deletes an existing blob upload that is currently in progress.
func (is *ImageStore) DeleteBlobUpload(repo, uuid string) error {
	blobUploadPath := is.BlobUploadPath(repo, uuid)

	writer, err := is.storeDriver.Writer(blobUploadPath, true)
	if err != nil {
		if errors.As(err, &driver.PathNotFoundError{}) {
			return zerr.ErrUploadNotFound
		}

		return err
	}

	defer writer.Close()

	if err := writer.Cancel(); err != nil {
		is.log.Error().Err(err).Str("blobUploadPath", blobUploadPath).Msg("error deleting blob upload")

		return err
	}

	return nil
}

// BlobPath returns the repository path of a blob.
func (is *ImageStore) BlobPath(repo string, digest godigest.Digest) string {
	return path.Join(is.rootDir, repo, "blobs", digest.Algorithm().String(), digest.Encoded())
}

/*
	CheckBlob verifies a blob and returns true if the blob is correct

If the blob is not found but it's found in cache then it will be copied over.
*/
func (is *ImageStore) CheckBlob(repo string, digest godigest.Digest) (bool, int64, error) {
	var lockLatency time.Time

	if err := digest.Validate(); err != nil {
		return false, -1, err
	}

	blobPath := is.BlobPath(repo, digest)

	if is.dedupe && fmt.Sprintf("%v", is.cache) != fmt.Sprintf("%v", nil) {
		is.Lock(&lockLatency)
		defer is.Unlock(&lockLatency)
	} else {
		is.RLock(&lockLatency)
		defer is.RUnlock(&lockLatency)
	}

	binfo, err := is.storeDriver.Stat(blobPath)
	if err == nil && binfo.Size() > 0 {
		is.log.Debug().Str("blob path", blobPath).Msg("blob path found")

		return true, binfo.Size(), nil
	}
	// otherwise is a 'deduped' blob (empty file)

	// Check blobs in cache
	dstRecord, err := is.checkCacheBlob(digest)
	if err != nil {
		is.log.Error().Err(err).Str("digest", digest.String()).Msg("cache: not found")

		return false, -1, zerr.ErrBlobNotFound
	}

	blobSize, err := is.copyBlob(repo, blobPath, dstRecord)
	if err != nil {
		return false, -1, zerr.ErrBlobNotFound
	}

	// put deduped blob in cache
	if err := is.cache.PutBlob(digest, blobPath); err != nil {
		is.log.Error().Err(err).Str("blobPath", blobPath).Msg("dedupe: unable to insert blob record")

		return false, -1, err
	}

	return true, blobSize, nil
}

// StatBlob verifies if a blob is present inside a repository. The caller function SHOULD lock from outside.
func (is *ImageStore) StatBlob(repo string, digest godigest.Digest) (bool, int64, time.Time, error) {
	if err := digest.Validate(); err != nil {
		return false, -1, time.Time{}, err
	}

	blobPath := is.BlobPath(repo, digest)

	binfo, err := is.storeDriver.Stat(blobPath)
	if err == nil && binfo.Size() > 0 {
		is.log.Debug().Str("blob path", blobPath).Msg("blob path found")

		return true, binfo.Size(), binfo.ModTime(), nil
	}

	if err != nil {
		is.log.Error().Err(err).Str("blob", blobPath).Msg("failed to stat blob")

		return false, -1, time.Time{}, zerr.ErrBlobNotFound
	}

	// then it's a 'deduped' blob

	// Check blobs in cache
	dstRecord, err := is.checkCacheBlob(digest)
	if err != nil {
		is.log.Error().Err(err).Str("digest", digest.String()).Msg("cache: not found")

		return false, -1, time.Time{}, zerr.ErrBlobNotFound
	}

	binfo, err = is.storeDriver.Stat(dstRecord)
	if err != nil {
		is.log.Error().Err(err).Str("blob", blobPath).Msg("failed to stat blob")

		return false, -1, time.Time{}, zerr.ErrBlobNotFound
	}

	return true, binfo.Size(), binfo.ModTime(), nil
}

func (is *ImageStore) checkCacheBlob(digest godigest.Digest) (string, error) {
	if err := digest.Validate(); err != nil {
		return "", err
	}

	if fmt.Sprintf("%v", is.cache) == fmt.Sprintf("%v", nil) {
		return "", zerr.ErrBlobNotFound
	}

	dstRecord, err := is.cache.GetBlob(digest)
	if err != nil {
		return "", err
	}

	if is.cache.UsesRelativePaths() {
		dstRecord = path.Join(is.rootDir, dstRecord)
	}

	if _, err := is.storeDriver.Stat(dstRecord); err != nil {
		is.log.Error().Err(err).Str("blob", dstRecord).Msg("failed to stat blob")

		// the actual blob on disk may have been removed by GC, so sync the cache
		if err := is.cache.DeleteBlob(digest, dstRecord); err != nil {
			is.log.Error().Err(err).Str("digest", digest.String()).Str("blobPath", dstRecord).
				Msg("unable to remove blob path from cache")

			return "", err
		}

		return "", zerr.ErrBlobNotFound
	}

	is.log.Debug().Str("digest", digest.String()).Str("dstRecord", dstRecord).Msg("cache: found dedupe record")

	return dstRecord, nil
}

func (is *ImageStore) copyBlob(repo string, blobPath, dstRecord string) (int64, error) {
	if err := is.initRepo(repo); err != nil {
		is.log.Error().Err(err).Str("repository", repo).Msg("unable to initialize an empty repo")

		return -1, err
	}

	_ = is.storeDriver.EnsureDir(filepath.Dir(blobPath))

	if err := is.storeDriver.Link(dstRecord, blobPath); err != nil {
		is.log.Error().Err(err).Str("blobPath", blobPath).Str("link", dstRecord).Msg("dedupe: unable to hard link")

		return -1, zerr.ErrBlobNotFound
	}

	// return original blob with content instead of the deduped one (blobPath)
	binfo, err := is.storeDriver.Stat(dstRecord)
	if err == nil {
		return binfo.Size(), nil
	}

	return -1, zerr.ErrBlobNotFound
}

// GetBlobPartial returns a partial stream to read the blob.
// blob selector instead of directly downloading the blob.
func (is *ImageStore) GetBlobPartial(repo string, digest godigest.Digest, mediaType string, from, to int64,
) (io.ReadCloser, int64, int64, error) {
	var lockLatency time.Time

	if err := digest.Validate(); err != nil {
		return nil, -1, -1, err
	}

	blobPath := is.BlobPath(repo, digest)

	is.RLock(&lockLatency)
	defer is.RUnlock(&lockLatency)

	binfo, err := is.storeDriver.Stat(blobPath)
	if err != nil {
		is.log.Error().Err(err).Str("blob", blobPath).Msg("failed to stat blob")

		return nil, -1, -1, zerr.ErrBlobNotFound
	}

	// is a deduped blob
	if binfo.Size() == 0 {
		// Check blobs in cache
		blobPath, err = is.checkCacheBlob(digest)
		if err != nil {
			is.log.Error().Err(err).Str("digest", digest.String()).Msg("cache: not found")

			return nil, -1, -1, zerr.ErrBlobNotFound
		}

		binfo, err = is.storeDriver.Stat(blobPath)
		if err != nil {
			is.log.Error().Err(err).Str("blob", blobPath).Msg("failed to stat blob")

			return nil, -1, -1, zerr.ErrBlobNotFound
		}
	}

	end := to

	if to < 0 || to >= binfo.Size() {
		end = binfo.Size() - 1
	}

	blobHandle, err := is.storeDriver.Reader(blobPath, from)
	if err != nil {
		is.log.Error().Err(err).Str("blob", blobPath).Msg("failed to open blob")

		return nil, -1, -1, err
	}

	blobReadCloser, err := newBlobStream(blobHandle, from, end)
	if err != nil {
		is.log.Error().Err(err).Str("blob", blobPath).Msg("failed to open blob stream")

		return nil, -1, -1, err
	}

	// The caller function is responsible for calling Close()
	return blobReadCloser, end - from + 1, binfo.Size(), nil
}

// GetBlob returns a stream to read the blob.
// blob selector instead of directly downloading the blob.
func (is *ImageStore) GetBlob(repo string, digest godigest.Digest, mediaType string) (io.ReadCloser, int64, error) {
	var lockLatency time.Time

	if err := digest.Validate(); err != nil {
		return nil, -1, err
	}

	blobPath := is.BlobPath(repo, digest)

	is.RLock(&lockLatency)
	defer is.RUnlock(&lockLatency)

	binfo, err := is.storeDriver.Stat(blobPath)
	if err != nil {
		is.log.Error().Err(err).Str("blob", blobPath).Msg("failed to stat blob")

		return nil, -1, zerr.ErrBlobNotFound
	}

	blobReadCloser, err := is.storeDriver.Reader(blobPath, 0)
	if err != nil {
		is.log.Error().Err(err).Str("blob", blobPath).Msg("failed to open blob")

		return nil, -1, err
	}

	// is a 'deduped' blob?
	if binfo.Size() == 0 {
		// Check blobs in cache
		dstRecord, err := is.checkCacheBlob(digest)
		if err != nil {
			is.log.Error().Err(err).Str("digest", digest.String()).Msg("cache: not found")

			return nil, -1, zerr.ErrBlobNotFound
		}

		binfo, err := is.storeDriver.Stat(dstRecord)
		if err != nil {
			is.log.Error().Err(err).Str("blob", dstRecord).Msg("failed to stat blob")

			return nil, -1, zerr.ErrBlobNotFound
		}

		blobReadCloser, err := is.storeDriver.Reader(dstRecord, 0)
		if err != nil {
			is.log.Error().Err(err).Str("blob", dstRecord).Msg("failed to open blob")

			return nil, -1, err
		}

		return blobReadCloser, binfo.Size(), nil
	}

	// The caller function is responsible for calling Close()
	return blobReadCloser, binfo.Size(), nil
}

// GetBlobContent returns blob contents, the caller function SHOULD lock from outside.
func (is *ImageStore) GetBlobContent(repo string, digest godigest.Digest) ([]byte, error) {
	if err := digest.Validate(); err != nil {
		return []byte{}, err
	}

	blobPath := is.BlobPath(repo, digest)

	binfo, err := is.storeDriver.Stat(blobPath)
	if err != nil {
		is.log.Error().Err(err).Str("blob", blobPath).Msg("failed to stat blob")

		return []byte{}, zerr.ErrBlobNotFound
	}

	blobBuf, err := is.storeDriver.ReadFile(blobPath)
	if err != nil {
		is.log.Error().Err(err).Str("blob", blobPath).Msg("failed to open blob")

		return nil, err
	}

	// is a 'deduped' blob?
	if binfo.Size() == 0 {
		// Check blobs in cache
		dstRecord, err := is.checkCacheBlob(digest)
		if err != nil {
			is.log.Error().Err(err).Str("digest", digest.String()).Msg("cache: not found")

			return nil, zerr.ErrBlobNotFound
		}

		blobBuf, err := is.storeDriver.ReadFile(dstRecord)
		if err != nil {
			is.log.Error().Err(err).Str("blob", dstRecord).Msg("failed to open blob")

			return nil, err
		}

		return blobBuf, nil
	}

	return blobBuf, nil
}

func (is *ImageStore) GetReferrers(repo string, gdigest godigest.Digest, artifactTypes []string,
) (ispec.Index, error) {
	var lockLatency time.Time

	is.RLock(&lockLatency)
	defer is.RUnlock(&lockLatency)

	return common.GetReferrers(is, repo, gdigest, artifactTypes, is.log)
}

func (is *ImageStore) GetOrasReferrers(repo string, gdigest godigest.Digest, artifactType string,
) ([]artifactspec.Descriptor, error) {
	var lockLatency time.Time

	is.RLock(&lockLatency)
	defer is.RUnlock(&lockLatency)

	return common.GetOrasReferrers(is, repo, gdigest, artifactType, is.log)
}

// GetIndexContent returns index.json contents, the caller function SHOULD lock from outside.
func (is *ImageStore) GetIndexContent(repo string) ([]byte, error) {
	dir := path.Join(is.rootDir, repo)

	buf, err := is.storeDriver.ReadFile(path.Join(dir, "index.json"))
	if err != nil {
		if errors.Is(err, driver.PathNotFoundError{}) {
			is.log.Error().Err(err).Str("dir", dir).Msg("index.json doesn't exist")

			return []byte{}, zerr.ErrRepoNotFound
		}

		is.log.Error().Err(err).Str("dir", dir).Msg("failed to read index.json")

		return []byte{}, err
	}

	return buf, nil
}

// DeleteBlob removes the blob from the repository.
func (is *ImageStore) DeleteBlob(repo string, digest godigest.Digest) error {
	var lockLatency time.Time

	if err := digest.Validate(); err != nil {
		return err
	}

	is.Lock(&lockLatency)
	defer is.Unlock(&lockLatency)

	return is.deleteBlob(repo, digest)
}

func (is *ImageStore) deleteBlob(repo string, digest godigest.Digest) error {
	blobPath := is.BlobPath(repo, digest)

	_, err := is.storeDriver.Stat(blobPath)
	if err != nil {
		is.log.Error().Err(err).Str("blob", blobPath).Msg("failed to stat blob")

		return zerr.ErrBlobNotFound
	}

	// first check if this blob is not currently in use
	if ok, _ := common.IsBlobReferenced(is, repo, digest, is.log); ok {
		return zerr.ErrBlobReferenced
	}

	if fmt.Sprintf("%v", is.cache) != fmt.Sprintf("%v", nil) {
		dstRecord, err := is.cache.GetBlob(digest)
		if err != nil && !errors.Is(err, zerr.ErrCacheMiss) {
			is.log.Error().Err(err).Str("blobPath", dstRecord).Msg("dedupe: unable to lookup blob record")

			return err
		}

		// remove cache entry and move blob contents to the next candidate if there is any
		if ok := is.cache.HasBlob(digest, blobPath); ok {
			if err := is.cache.DeleteBlob(digest, blobPath); err != nil {
				is.log.Error().Err(err).Str("digest", digest.String()).Str("blobPath", blobPath).
					Msg("unable to remove blob path from cache")

				return err
			}
		}

		// if the deleted blob is one with content
		if dstRecord == blobPath {
			// get next candidate
			dstRecord, err := is.cache.GetBlob(digest)
			if err != nil && !errors.Is(err, zerr.ErrCacheMiss) {
				is.log.Error().Err(err).Str("blobPath", dstRecord).Msg("dedupe: unable to lookup blob record")

				return err
			}

			// if we have a new candidate move the blob content to it
			if dstRecord != "" {
				/* check to see if we need to move the content from original blob to duplicate one
				(in case of filesystem, this should not be needed */
				binfo, err := is.storeDriver.Stat(dstRecord)
				if err != nil {
					is.log.Error().Err(err).Str("path", blobPath).Msg("rebuild dedupe: failed to stat blob")

					return err
				}

				if binfo.Size() == 0 {
					if err := is.storeDriver.Move(blobPath, dstRecord); err != nil {
						is.log.Error().Err(err).Str("blobPath", blobPath).Msg("unable to remove blob path")

						return err
					}
				}

				return nil
			}
		}
	}

	if err := is.storeDriver.Delete(blobPath); err != nil {
		is.log.Error().Err(err).Str("blobPath", blobPath).Msg("unable to remove blob path")

		return err
	}

	return nil
}

func (is *ImageStore) garbageCollect(repo string) error {
	if is.gcReferrers {
		is.log.Info().Msg("gc: manifests with missing referrers")

		// gc all manifests that have a missing subject, stop when no gc happened in a full loop over index.json.
		stop := false
		for !stop {
			// because we gc manifests in the loop, need to get latest index.json content
			index, err := common.GetIndex(is, repo, is.log)
			if err != nil {
				return err
			}

			gced, err := is.garbageCollectIndexReferrers(repo, index, index)
			if err != nil {
				return err
			}

			/* if we delete any manifest then loop again and gc manifests with
			a subject pointing to the last ones which were gc'ed. */
			stop = !gced
		}
	}

	index, err := common.GetIndex(is, repo, is.log)
	if err != nil {
		return err
	}

	is.log.Info().Msg("gc: manifests without tags")

	// apply image retention policy
	if err := is.garbageCollectUntaggedManifests(index, repo); err != nil {
		return err
	}

	is.log.Info().Msg("gc: blobs")

	if err := is.garbageCollectBlobs(is, repo, is.gcDelay, is.log); err != nil {
		return err
	}

	return nil
}

/*
garbageCollectIndexReferrers will gc all referrers with a missing subject recursively

rootIndex is indexJson, need to pass it down to garbageCollectReferrer()
rootIndex is the place we look for referrers.
*/
func (is *ImageStore) garbageCollectIndexReferrers(repo string, rootIndex ispec.Index, index ispec.Index,
) (bool, error) {
	var count int

	var err error

	for _, desc := range index.Manifests {
		switch desc.MediaType {
		case ispec.MediaTypeImageIndex:
			indexImage, err := common.GetImageIndex(is, repo, desc.Digest, is.log)
			if err != nil {
				is.log.Error().Err(err).Str("repository", repo).Str("digest", desc.Digest.String()).
					Msg("gc: failed to read multiarch(index) image")

				return false, err
			}

			gced, err := is.garbageCollectReferrer(repo, rootIndex, desc, indexImage.Subject)
			if err != nil {
				return false, err
			}

			/* if we gc index then no need to continue searching for referrers inside it.
			they will be gced when the next garbage collect is executed(if they are older than retentionDelay),
			 because manifests part of indexes will still be referenced in index.json */
			if gced {
				return true, nil
			}

			if gced, err = is.garbageCollectIndexReferrers(repo, rootIndex, indexImage); err != nil {
				return false, err
			}

			if gced {
				count++
			}

		case ispec.MediaTypeImageManifest, artifactspec.MediaTypeArtifactManifest:
			image, err := common.GetImageManifest(is, repo, desc.Digest, is.log)
			if err != nil {
				is.log.Error().Err(err).Str("repo", repo).Str("digest", desc.Digest.String()).
					Msg("gc: failed to read manifest image")

				return false, err
			}

			gced, err := is.garbageCollectReferrer(repo, rootIndex, desc, image.Subject)
			if err != nil {
				return false, err
			}

			if gced {
				count++
			}
		}
	}

	return count > 0, err
}

func (is *ImageStore) garbageCollectReferrer(repo string, index ispec.Index, manifestDesc ispec.Descriptor,
	subject *ispec.Descriptor,
) (bool, error) {
	var gced bool

	var err error

	if subject != nil {
		// try to find subject in index.json
		if ok := isManifestReferencedInIndex(index, subject.Digest); !ok {
			gced, err = garbageCollectManifest(is, repo, manifestDesc.Digest, is.gcDelay)
			if err != nil {
				return false, err
			}
		}
	}

	tag, ok := manifestDesc.Annotations[ispec.AnnotationRefName]
	if ok {
		if strings.HasPrefix(tag, "sha256-") && (strings.HasSuffix(tag, cosignSignatureTagSuffix) ||
			strings.HasSuffix(tag, SBOMTagSuffix)) {
			if ok := isManifestReferencedInIndex(index, getSubjectFromCosignTag(tag)); !ok {
				gced, err = garbageCollectManifest(is, repo, manifestDesc.Digest, is.gcDelay)
				if err != nil {
					return false, err
				}
			}
		}
	}

	return gced, err
}

func (is *ImageStore) garbageCollectUntaggedManifests(index ispec.Index, repo string) error {
	referencedByImageIndex := make([]string, 0)

	if err := identifyManifestsReferencedInIndex(is, index, repo, &referencedByImageIndex); err != nil {
		return err
	}

	// first gather manifests part of image indexes and referrers, we want to skip checking them
	for _, desc := range index.Manifests {
		// skip manifests referenced in image indexes
		if zcommon.Contains(referencedByImageIndex, desc.Digest.String()) {
			continue
		}

		// remove untagged images
		if desc.MediaType == ispec.MediaTypeImageManifest || desc.MediaType == ispec.MediaTypeImageIndex {
			_, ok := desc.Annotations[ispec.AnnotationRefName]
			if !ok {
				_, err := garbageCollectManifest(is, repo, desc.Digest, is.retentionDelay)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// Adds both referenced manifests and referrers from an index.
func identifyManifestsReferencedInIndex(imgStore *ImageStore, index ispec.Index, repo string, referenced *[]string,
) error {
	for _, desc := range index.Manifests {
		switch desc.MediaType {
		case ispec.MediaTypeImageIndex:
			indexImage, err := common.GetImageIndex(imgStore, repo, desc.Digest, imgStore.log)
			if err != nil {
				imgStore.log.Error().Err(err).Str("repository", repo).Str("digest", desc.Digest.String()).
					Msg("gc: failed to read multiarch(index) image")

				return err
			}

			if indexImage.Subject != nil {
				*referenced = append(*referenced, desc.Digest.String())
			}

			for _, indexDesc := range indexImage.Manifests {
				*referenced = append(*referenced, indexDesc.Digest.String())
			}

			if err := identifyManifestsReferencedInIndex(imgStore, indexImage, repo, referenced); err != nil {
				return err
			}
		case ispec.MediaTypeImageManifest, artifactspec.MediaTypeArtifactManifest:
			image, err := common.GetImageManifest(imgStore, repo, desc.Digest, imgStore.log)
			if err != nil {
				imgStore.log.Error().Err(err).Str("repo", repo).Str("digest", desc.Digest.String()).
					Msg("gc: failed to read manifest image")

				return err
			}

			if image.Subject != nil {
				*referenced = append(*referenced, desc.Digest.String())
			}
		}
	}

	return nil
}

func garbageCollectManifest(imgStore *ImageStore, repo string, digest godigest.Digest, delay time.Duration,
) (bool, error) {
	canGC, err := isBlobOlderThan(imgStore, repo, digest, delay, imgStore.log)
	if err != nil {
		imgStore.log.Error().Err(err).Str("repository", repo).Str("digest", digest.String()).
			Str("delay", imgStore.gcDelay.String()).Msg("gc: failed to check if blob is older than delay")

		return false, err
	}

	if canGC {
		imgStore.log.Info().Str("repository", repo).Str("digest", digest.String()).
			Msg("gc: removing unreferenced manifest")

		if err := imgStore.deleteImageManifest(repo, digest.String(), true); err != nil {
			if errors.Is(err, zerr.ErrManifestConflict) {
				imgStore.log.Info().Str("repository", repo).Str("digest", digest.String()).
					Msg("gc: skipping removing manifest due to conflict")

				return false, nil
			}

			return false, err
		}

		return true, nil
	}

	return false, nil
}

func (is *ImageStore) garbageCollectBlobs(imgStore *ImageStore, repo string,
	delay time.Duration, log zlog.Logger,
) error {
	refBlobs := map[string]bool{}

	err := common.AddRepoBlobsToReferences(imgStore, repo, refBlobs, log)
	if err != nil {
		log.Error().Err(err).Str("repository", repo).Msg("unable to get referenced blobs in repo")

		return err
	}

	allBlobs, err := imgStore.GetAllBlobs(repo)
	if err != nil {
		// /blobs/sha256/ may be empty in the case of s3, no need to return err, we want to skip
		if errors.As(err, &driver.PathNotFoundError{}) {
			return nil
		}

		log.Error().Err(err).Str("repository", repo).Msg("unable to get all blobs")

		return err
	}

	reaped := 0

	for _, blob := range allBlobs {
		digest := godigest.NewDigestFromEncoded(godigest.SHA256, blob)
		if err = digest.Validate(); err != nil {
			log.Error().Err(err).Str("repository", repo).Str("digest", blob).Msg("unable to parse digest")

			return err
		}

		if _, ok := refBlobs[digest.String()]; !ok {
			ok, err := isBlobOlderThan(imgStore, repo, digest, delay, log)
			if err != nil {
				log.Error().Err(err).Str("repository", repo).Str("digest", blob).Msg("unable to determine GC delay")

				return err
			}

			if !ok {
				continue
			}

			if err := imgStore.deleteBlob(repo, digest); err != nil {
				if errors.Is(err, zerr.ErrBlobReferenced) {
					if err := imgStore.deleteImageManifest(repo, digest.String(), true); err != nil {
						if errors.Is(err, zerr.ErrManifestConflict) {
							continue
						}

						log.Error().Err(err).Str("repository", repo).Str("digest", blob).Msg("unable to delete blob")

						return err
					}
				} else {
					log.Error().Err(err).Str("repository", repo).Str("digest", blob).Msg("unable to delete blob")

					return err
				}
			}

			log.Info().Str("repository", repo).Str("digest", blob).Msg("garbage collected blob")

			reaped++
		}
	}

	// if we cleaned all blobs let's also remove the repo so that it won't be returned by catalog
	if reaped == len(allBlobs) {
		log.Info().Str("repository", repo).Msg("garbage collected all blobs, cleaning repo...")

		if err := is.storeDriver.Delete(path.Join(is.rootDir, repo)); err != nil {
			log.Error().Err(err).Str("repository", repo).Msg("unable to delete repo")

			return err
		}
	}

	log.Info().Str("repository", repo).Int("count", reaped).Msg("garbage collected blobs")

	return nil
}

func (is *ImageStore) gcRepo(repo string) error {
	var lockLatency time.Time

	is.Lock(&lockLatency)
	err := is.garbageCollect(repo)
	is.Unlock(&lockLatency)

	if err != nil {
		return err
	}

	return nil
}

func (is *ImageStore) GetAllBlobs(repo string) ([]string, error) {
	dir := path.Join(is.rootDir, repo, "blobs", "sha256")

	files, err := is.storeDriver.List(dir)
	if err != nil {
		return []string{}, err
	}

	ret := []string{}

	for _, file := range files {
		ret = append(ret, filepath.Base(file))
	}

	return ret, nil
}

func (is *ImageStore) RunGCRepo(repo string) error {
	is.log.Info().Msg(fmt.Sprintf("executing GC of orphaned blobs for %s", path.Join(is.RootDir(), repo)))

	if err := is.gcRepo(repo); err != nil {
		errMessage := fmt.Sprintf("error while running GC for %s", path.Join(is.RootDir(), repo))
		is.log.Error().Err(err).Msg(errMessage)
		is.log.Info().Msg(fmt.Sprintf("GC unsuccessfully completed for %s", path.Join(is.RootDir(), repo)))

		return err
	}

	is.log.Info().Msg(fmt.Sprintf("GC successfully completed for %s", path.Join(is.RootDir(), repo)))

	return nil
}

func (is *ImageStore) RunGCPeriodically(interval time.Duration, sch *scheduler.Scheduler) {
	generator := &common.GCTaskGenerator{
		ImgStore: is,
	}

	sch.SubmitGenerator(generator, interval, scheduler.MediumPriority)
}

func (is *ImageStore) GetNextDigestWithBlobPaths(lastDigests []godigest.Digest) (godigest.Digest, []string, error) {
	var lockLatency time.Time

	dir := is.rootDir

	is.RLock(&lockLatency)
	defer is.RUnlock(&lockLatency)

	var duplicateBlobs []string

	var digest godigest.Digest

	err := is.storeDriver.Walk(dir, func(fileInfo driver.FileInfo) error {
		// skip blobs under .sync
		if strings.HasSuffix(fileInfo.Path(), syncConstants.SyncBlobUploadDir) {
			return driver.ErrSkipDir
		}

		if fileInfo.IsDir() {
			return nil
		}

		blobDigest := godigest.NewDigestFromEncoded("sha256", path.Base(fileInfo.Path()))
		if err := blobDigest.Validate(); err != nil { //nolint: nilerr
			return nil //nolint: nilerr // ignore files which are not blobs
		}

		if digest == "" && !zcommon.Contains(lastDigests, blobDigest) {
			digest = blobDigest
		}

		if blobDigest == digest {
			duplicateBlobs = append(duplicateBlobs, fileInfo.Path())
		}

		return nil
	})

	// if the root directory is not yet created
	var perr driver.PathNotFoundError

	if errors.As(err, &perr) {
		return digest, duplicateBlobs, nil
	}

	return digest, duplicateBlobs, err
}

func (is *ImageStore) getOriginalBlobFromDisk(duplicateBlobs []string) (string, error) {
	for _, blobPath := range duplicateBlobs {
		binfo, err := is.storeDriver.Stat(blobPath)
		if err != nil {
			is.log.Error().Err(err).Str("path", blobPath).Msg("rebuild dedupe: failed to stat blob")

			return "", zerr.ErrBlobNotFound
		}

		if binfo.Size() > 0 {
			return blobPath, nil
		}
	}

	return "", zerr.ErrBlobNotFound
}

func (is *ImageStore) getOriginalBlob(digest godigest.Digest, duplicateBlobs []string) (string, error) {
	var originalBlob string

	var err error

	originalBlob, err = is.checkCacheBlob(digest)
	if err != nil && !errors.Is(err, zerr.ErrBlobNotFound) && !errors.Is(err, zerr.ErrCacheMiss) {
		is.log.Error().Err(err).Msg("rebuild dedupe: unable to find blob in cache")

		return originalBlob, err
	}

	// if we still don't have, search it
	if originalBlob == "" {
		is.log.Warn().Msg("rebuild dedupe: failed to find blob in cache, searching it in storage...")
		// a rebuild dedupe was attempted in the past
		// get original blob, should be found otherwise exit with error

		originalBlob, err = is.getOriginalBlobFromDisk(duplicateBlobs)
		if err != nil {
			return originalBlob, err
		}
	}

	is.log.Info().Str("originalBlob", originalBlob).Msg("rebuild dedupe: found original blob")

	return originalBlob, nil
}

func (is *ImageStore) dedupeBlobs(digest godigest.Digest, duplicateBlobs []string) error {
	if fmt.Sprintf("%v", is.cache) == fmt.Sprintf("%v", nil) {
		is.log.Error().Err(zerr.ErrDedupeRebuild).Msg("no cache driver found, can not dedupe blobs")

		return zerr.ErrDedupeRebuild
	}

	is.log.Info().Str("digest", digest.String()).Msg("rebuild dedupe: deduping blobs for digest")

	var originalBlob string

	// rebuild from dedupe false to true
	for _, blobPath := range duplicateBlobs {
		binfo, err := is.storeDriver.Stat(blobPath)
		if err != nil {
			is.log.Error().Err(err).Str("path", blobPath).Msg("rebuild dedupe: failed to stat blob")

			return err
		}

		if binfo.Size() == 0 {
			is.log.Warn().Msg("rebuild dedupe: found file without content, trying to find the original blob")
			// a rebuild dedupe was attempted in the past
			// get original blob, should be found otherwise exit with error
			if originalBlob == "" {
				originalBlob, err = is.getOriginalBlob(digest, duplicateBlobs)
				if err != nil {
					is.log.Error().Err(err).Msg("rebuild dedupe: unable to find original blob")

					return zerr.ErrDedupeRebuild
				}

				// cache original blob
				if ok := is.cache.HasBlob(digest, originalBlob); !ok {
					if err := is.cache.PutBlob(digest, originalBlob); err != nil {
						return err
					}
				}
			}

			// cache dedupe blob
			if ok := is.cache.HasBlob(digest, blobPath); !ok {
				if err := is.cache.PutBlob(digest, blobPath); err != nil {
					return err
				}
			}
		} else {
			// if we have an original blob cached then we can safely dedupe the rest of them
			if originalBlob != "" {
				if err := is.storeDriver.Link(originalBlob, blobPath); err != nil {
					is.log.Error().Err(err).Str("path", blobPath).Msg("rebuild dedupe: unable to dedupe blob")

					return err
				}
			}

			// cache it
			if ok := is.cache.HasBlob(digest, blobPath); !ok {
				if err := is.cache.PutBlob(digest, blobPath); err != nil {
					return err
				}
			}

			// mark blob as preserved
			originalBlob = blobPath
		}
	}

	is.log.Info().Str("digest", digest.String()).Msg("rebuild dedupe: deduping blobs for digest finished successfully")

	return nil
}

func (is *ImageStore) restoreDedupedBlobs(digest godigest.Digest, duplicateBlobs []string) error {
	is.log.Info().Str("digest", digest.String()).Msg("rebuild dedupe: restoring deduped blobs for digest")

	// first we need to find the original blob, either in cache or by checking each blob size
	originalBlob, err := is.getOriginalBlob(digest, duplicateBlobs)
	if err != nil {
		is.log.Error().Err(err).Msg("rebuild dedupe: unable to find original blob")

		return zerr.ErrDedupeRebuild
	}

	for _, blobPath := range duplicateBlobs {
		binfo, err := is.storeDriver.Stat(blobPath)
		if err != nil {
			is.log.Error().Err(err).Str("path", blobPath).Msg("rebuild dedupe: failed to stat blob")

			return err
		}

		// if we find a deduped blob, then copy original blob content to deduped one
		if binfo.Size() == 0 {
			// move content from original blob to deduped one
			buf, err := is.storeDriver.ReadFile(originalBlob)
			if err != nil {
				is.log.Error().Err(err).Str("path", originalBlob).Msg("rebuild dedupe: failed to get original blob content")

				return err
			}

			_, err = is.storeDriver.WriteFile(blobPath, buf)
			if err != nil {
				return err
			}
		}
	}

	is.log.Info().Str("digest", digest.String()).
		Msg("rebuild dedupe: restoring deduped blobs for digest finished successfully")

	return nil
}

func (is *ImageStore) RunDedupeForDigest(digest godigest.Digest, dedupe bool, duplicateBlobs []string) error {
	var lockLatency time.Time

	is.Lock(&lockLatency)
	defer is.Unlock(&lockLatency)

	if dedupe {
		return is.dedupeBlobs(digest, duplicateBlobs)
	}

	return is.restoreDedupedBlobs(digest, duplicateBlobs)
}

func (is *ImageStore) RunDedupeBlobs(interval time.Duration, sch *scheduler.Scheduler) {
	generator := &common.DedupeTaskGenerator{
		ImgStore: is,
		Dedupe:   is.dedupe,
		Log:      is.log,
	}

	sch.SubmitGenerator(generator, interval, scheduler.MediumPriority)
}

type blobStream struct {
	reader io.Reader
	closer io.Closer
}

func newBlobStream(readCloser io.ReadCloser, from, to int64) (io.ReadCloser, error) {
	if from < 0 || to < from {
		return nil, zerr.ErrBadRange
	}

	return &blobStream{reader: io.LimitReader(readCloser, to-from+1), closer: readCloser}, nil
}

func (bs *blobStream) Read(buf []byte) (int, error) {
	return bs.reader.Read(buf)
}

func (bs *blobStream) Close() error {
	return bs.closer.Close()
}

func isBlobOlderThan(imgStore storageTypes.ImageStore, repo string,
	digest godigest.Digest, delay time.Duration, log zlog.Logger,
) (bool, error) {
	_, _, modtime, err := imgStore.StatBlob(repo, digest)
	if err != nil {
		log.Error().Err(err).Str("repository", repo).Str("digest", digest.String()).
			Msg("gc: failed to stat blob")

		return false, err
	}

	if modtime.Add(delay).After(time.Now()) {
		return false, nil
	}

	log.Info().Str("repository", repo).Str("digest", digest.String()).Msg("perform GC on blob")

	return true, nil
}

func getSubjectFromCosignTag(tag string) godigest.Digest {
	alg := strings.Split(tag, "-")[0]
	encoded := strings.Split(strings.Split(tag, "-")[1], ".sig")[0]

	return godigest.NewDigestFromEncoded(godigest.Algorithm(alg), encoded)
}

func isManifestReferencedInIndex(index ispec.Index, digest godigest.Digest) bool {
	for _, manifest := range index.Manifests {
		if manifest.Digest == digest {
			return true
		}
	}

	return false
}
