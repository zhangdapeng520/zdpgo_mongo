// Copyright (C) MongoDB, Inc. 2017-present.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

package gridfs // import "github.com/zhangdapeng520/zdpgo_mongo/mongo/gridfs"

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/zhangdapeng520/zdpgo_mongo/bson"
	"github.com/zhangdapeng520/zdpgo_mongo/bson/primitive"
	"github.com/zhangdapeng520/zdpgo_mongo/internal/csot"
	"github.com/zhangdapeng520/zdpgo_mongo/mongo"
	"github.com/zhangdapeng520/zdpgo_mongo/mongo/options"
	"github.com/zhangdapeng520/zdpgo_mongo/mongo/readconcern"
	"github.com/zhangdapeng520/zdpgo_mongo/mongo/readpref"
	"github.com/zhangdapeng520/zdpgo_mongo/mongo/writeconcern"
	"github.com/zhangdapeng520/zdpgo_mongo/x/bsonx/bsoncore"
)

// TODO: add sessions options

// DefaultChunkSize is the default size of each file chunk.
const DefaultChunkSize int32 = 255 * 1024 // 255 KiB

// ErrFileNotFound occurs if a user asks to download a file with a file ID that isn't found in the files collection.
var ErrFileNotFound = errors.New("file with given parameters not found")

// ErrMissingChunkSize occurs when downloading a file if the files collection document is missing the "chunkSize" field.
var ErrMissingChunkSize = errors.New("files collection document does not contain a 'chunkSize' field")

// Bucket represents a GridFS bucket.
type Bucket struct {
	db         *mongo.Database
	chunksColl *mongo.Collection // collection to store file chunks
	filesColl  *mongo.Collection // collection to store file metadata

	name      string
	chunkSize int32
	wc        *writeconcern.WriteConcern
	rc        *readconcern.ReadConcern
	rp        *readpref.ReadPref

	firstWriteDone bool
	readBuf        []byte
	writeBuf       []byte

	readDeadline  time.Time
	writeDeadline time.Time
}

// Upload contains options to upload a file to a bucket.
type Upload struct {
	chunkSize int32
	metadata  bson.D
}

// NewBucket creates a GridFS bucket.
func NewBucket(db *mongo.Database, opts ...*options.BucketOptions) (*Bucket, error) {
	b := &Bucket{
		name:      "fs",
		chunkSize: DefaultChunkSize,
		db:        db,
		wc:        db.WriteConcern(),
		rc:        db.ReadConcern(),
		rp:        db.ReadPreference(),
	}

	bo := options.MergeBucketOptions(opts...)
	if bo.Name != nil {
		b.name = *bo.Name
	}
	if bo.ChunkSizeBytes != nil {
		b.chunkSize = *bo.ChunkSizeBytes
	}
	if bo.WriteConcern != nil {
		b.wc = bo.WriteConcern
	}
	if bo.ReadConcern != nil {
		b.rc = bo.ReadConcern
	}
	if bo.ReadPreference != nil {
		b.rp = bo.ReadPreference
	}

	var collOpts = options.Collection().SetWriteConcern(b.wc).SetReadConcern(b.rc).SetReadPreference(b.rp)

	b.chunksColl = db.Collection(b.name+".chunks", collOpts)
	b.filesColl = db.Collection(b.name+".files", collOpts)
	b.readBuf = make([]byte, b.chunkSize)
	b.writeBuf = make([]byte, b.chunkSize)

	return b, nil
}

// SetWriteDeadline sets the write deadline for this bucket.
func (b *Bucket) SetWriteDeadline(t time.Time) error {
	b.writeDeadline = t
	return nil
}

// SetReadDeadline sets the read deadline for this bucket
func (b *Bucket) SetReadDeadline(t time.Time) error {
	b.readDeadline = t
	return nil
}

// OpenUploadStream creates a file ID new upload stream for a file given the filename.
func (b *Bucket) OpenUploadStream(filename string, opts ...*options.UploadOptions) (*UploadStream, error) {
	return b.OpenUploadStreamWithID(primitive.NewObjectID(), filename, opts...)
}

// OpenUploadStreamWithID creates a new upload stream for a file given the file ID and filename.
func (b *Bucket) OpenUploadStreamWithID(fileID interface{}, filename string, opts ...*options.UploadOptions) (*UploadStream, error) {
	ctx, cancel := deadlineContext(b.writeDeadline)
	if cancel != nil {
		defer cancel()
	}

	if err := b.checkFirstWrite(ctx); err != nil {
		return nil, err
	}

	upload, err := b.parseUploadOptions(opts...)
	if err != nil {
		return nil, err
	}

	return newUploadStream(upload, fileID, filename, b.chunksColl, b.filesColl), nil
}

// UploadFromStream creates a fileID and uploads a file given a source stream.
//
// If this upload requires a custom write deadline to be set on the bucket, it cannot be done concurrently with other
// write operations operations on this bucket that also require a custom deadline.
func (b *Bucket) UploadFromStream(filename string, source io.Reader, opts ...*options.UploadOptions) (primitive.ObjectID, error) {
	fileID := primitive.NewObjectID()
	err := b.UploadFromStreamWithID(fileID, filename, source, opts...)
	return fileID, err
}

// UploadFromStreamWithID uploads a file given a source stream.
//
// If this upload requires a custom write deadline to be set on the bucket, it cannot be done concurrently with other
// write operations operations on this bucket that also require a custom deadline.
func (b *Bucket) UploadFromStreamWithID(fileID interface{}, filename string, source io.Reader, opts ...*options.UploadOptions) error {
	us, err := b.OpenUploadStreamWithID(fileID, filename, opts...)
	if err != nil {
		return err
	}

	err = us.SetWriteDeadline(b.writeDeadline)
	if err != nil {
		_ = us.Close()
		return err
	}

	for {
		n, err := source.Read(b.readBuf)
		if err != nil && err != io.EOF {
			_ = us.Abort() // upload considered aborted if source stream returns an error
			return err
		}

		if n > 0 {
			_, err := us.Write(b.readBuf[:n])
			if err != nil {
				return err
			}
		}

		if n == 0 || err == io.EOF {
			break
		}
	}

	return us.Close()
}

// OpenDownloadStream creates a stream from which the contents of the file can be read.
func (b *Bucket) OpenDownloadStream(fileID interface{}) (*DownloadStream, error) {
	return b.openDownloadStream(bson.D{
		{"_id", fileID},
	})
}

// DownloadToStream downloads the file with the specified fileID and writes it to the provided io.Writer.
// Returns the number of bytes written to the stream and an error, or nil if there was no error.
//
// If this download requires a custom read deadline to be set on the bucket, it cannot be done concurrently with other
// read operations operations on this bucket that also require a custom deadline.
func (b *Bucket) DownloadToStream(fileID interface{}, stream io.Writer) (int64, error) {
	ds, err := b.OpenDownloadStream(fileID)
	if err != nil {
		return 0, err
	}

	return b.downloadToStream(ds, stream)
}

// OpenDownloadStreamByName opens a download stream for the file with the given filename.
func (b *Bucket) OpenDownloadStreamByName(filename string, opts ...*options.NameOptions) (*DownloadStream, error) {
	var numSkip int32 = -1
	var sortOrder int32 = 1

	nameOpts := options.MergeNameOptions(opts...)
	if nameOpts.Revision != nil {
		numSkip = *nameOpts.Revision
	}

	if numSkip < 0 {
		sortOrder = -1
		numSkip = (-1 * numSkip) - 1
	}

	findOpts := options.Find().SetSkip(int64(numSkip)).SetSort(bson.D{{"uploadDate", sortOrder}})

	return b.openDownloadStream(bson.D{{"filename", filename}}, findOpts)
}

// DownloadToStreamByName downloads the file with the given name to the given io.Writer.
//
// If this download requires a custom read deadline to be set on the bucket, it cannot be done concurrently with other
// read operations operations on this bucket that also require a custom deadline.
func (b *Bucket) DownloadToStreamByName(filename string, stream io.Writer, opts ...*options.NameOptions) (int64, error) {
	ds, err := b.OpenDownloadStreamByName(filename, opts...)
	if err != nil {
		return 0, err
	}

	return b.downloadToStream(ds, stream)
}

// Delete deletes all chunks and metadata associated with the file with the given file ID.
//
// If this operation requires a custom write deadline to be set on the bucket, it cannot be done concurrently with other
// write operations operations on this bucket that also require a custom deadline.
//
// Use SetWriteDeadline to set a deadline for the delete operation.
func (b *Bucket) Delete(fileID interface{}) error {
	ctx, cancel := deadlineContext(b.writeDeadline)
	if cancel != nil {
		defer cancel()
	}
	return b.DeleteContext(ctx, fileID)
}

// DeleteContext deletes all chunks and metadata associated with the file with the given file ID and runs the underlying
// delete operations with the provided context.
//
// Use the context parameter to time-out or cancel the delete operation. The deadline set by SetWriteDeadline is ignored.
func (b *Bucket) DeleteContext(ctx context.Context, fileID interface{}) error {
	// If Timeout is set on the Client and context is not already a Timeout
	// context, honor Timeout in new Timeout context for operation execution to
	// be shared by both delete operations.
	if b.db.Client().Timeout() != nil && !csot.IsTimeoutContext(ctx) {
		newCtx, cancelFunc := csot.MakeTimeoutContext(ctx, *b.db.Client().Timeout())
		// Redefine ctx to be the new timeout-derived context.
		ctx = newCtx
		// Cancel the timeout-derived context at the end of Execute to avoid a context leak.
		defer cancelFunc()
	}

	// Delete document in files collection and then chunks to minimize race conditions.
	res, err := b.filesColl.DeleteOne(ctx, bson.D{{"_id", fileID}})
	if err == nil && res.DeletedCount == 0 {
		err = ErrFileNotFound
	}
	if err != nil {
		_ = b.deleteChunks(ctx, fileID) // Can attempt to delete chunks even if no docs in files collection matched.
		return err
	}

	return b.deleteChunks(ctx, fileID)
}

// Find returns the files collection documents that match the given filter.
//
// If this download requires a custom read deadline to be set on the bucket, it cannot be done concurrently with other
// read operations operations on this bucket that also require a custom deadline.
//
// Use SetReadDeadline to set a deadline for the find operation.
func (b *Bucket) Find(filter interface{}, opts ...*options.GridFSFindOptions) (*mongo.Cursor, error) {
	ctx, cancel := deadlineContext(b.readDeadline)
	if cancel != nil {
		defer cancel()
	}

	return b.FindContext(ctx, filter, opts...)
}

// FindContext returns the files collection documents that match the given filter and runs the underlying
// find query with the provided context.
//
// Use the context parameter to time-out or cancel the find operation. The deadline set by SetReadDeadline
// is ignored.
func (b *Bucket) FindContext(ctx context.Context, filter interface{}, opts ...*options.GridFSFindOptions) (*mongo.Cursor, error) {
	gfsOpts := options.MergeGridFSFindOptions(opts...)
	find := options.Find()
	if gfsOpts.AllowDiskUse != nil {
		find.SetAllowDiskUse(*gfsOpts.AllowDiskUse)
	}
	if gfsOpts.BatchSize != nil {
		find.SetBatchSize(*gfsOpts.BatchSize)
	}
	if gfsOpts.Limit != nil {
		find.SetLimit(int64(*gfsOpts.Limit))
	}
	if gfsOpts.MaxTime != nil {
		find.SetMaxTime(*gfsOpts.MaxTime)
	}
	if gfsOpts.NoCursorTimeout != nil {
		find.SetNoCursorTimeout(*gfsOpts.NoCursorTimeout)
	}
	if gfsOpts.Skip != nil {
		find.SetSkip(int64(*gfsOpts.Skip))
	}
	if gfsOpts.Sort != nil {
		find.SetSort(gfsOpts.Sort)
	}

	return b.filesColl.Find(ctx, filter, find)
}

// Rename renames the stored file with the specified file ID.
//
// If this operation requires a custom write deadline to be set on the bucket, it cannot be done concurrently with other
// write operations operations on this bucket that also require a custom deadline
//
// Use SetWriteDeadline to set a deadline for the rename operation.
func (b *Bucket) Rename(fileID interface{}, newFilename string) error {
	ctx, cancel := deadlineContext(b.writeDeadline)
	if cancel != nil {
		defer cancel()
	}

	return b.RenameContext(ctx, fileID, newFilename)
}

// RenameContext renames the stored file with the specified file ID and runs the underlying update with the provided
// context.
//
// Use the context parameter to time-out or cancel the rename operation. The deadline set by SetWriteDeadline is ignored.
func (b *Bucket) RenameContext(ctx context.Context, fileID interface{}, newFilename string) error {
	res, err := b.filesColl.UpdateOne(ctx,
		bson.D{{"_id", fileID}},
		bson.D{{"$set", bson.D{{"filename", newFilename}}}},
	)
	if err != nil {
		return err
	}

	if res.MatchedCount == 0 {
		return ErrFileNotFound
	}

	return nil
}

// Drop drops the files and chunks collections associated with this bucket.
//
// If this operation requires a custom write deadline to be set on the bucket, it cannot be done concurrently with other
// write operations operations on this bucket that also require a custom deadline
//
// Use SetWriteDeadline to set a deadline for the drop operation.
func (b *Bucket) Drop() error {
	ctx, cancel := deadlineContext(b.writeDeadline)
	if cancel != nil {
		defer cancel()
	}

	return b.DropContext(ctx)
}

// DropContext drops the files and chunks collections associated with this bucket and runs the drop operations with
// the provided context.
//
// Use the context parameter to time-out or cancel the drop operation. The deadline set by SetWriteDeadline is ignored.
func (b *Bucket) DropContext(ctx context.Context) error {
	// If Timeout is set on the Client and context is not already a Timeout
	// context, honor Timeout in new Timeout context for operation execution to
	// be shared by both drop operations.
	if b.db.Client().Timeout() != nil && !csot.IsTimeoutContext(ctx) {
		newCtx, cancelFunc := csot.MakeTimeoutContext(ctx, *b.db.Client().Timeout())
		// Redefine ctx to be the new timeout-derived context.
		ctx = newCtx
		// Cancel the timeout-derived context at the end of Execute to avoid a context leak.
		defer cancelFunc()
	}

	err := b.filesColl.Drop(ctx)
	if err != nil {
		return err
	}

	return b.chunksColl.Drop(ctx)
}

// GetFilesCollection returns a handle to the collection that stores the file documents for this bucket.
func (b *Bucket) GetFilesCollection() *mongo.Collection {
	return b.filesColl
}

// GetChunksCollection returns a handle to the collection that stores the file chunks for this bucket.
func (b *Bucket) GetChunksCollection() *mongo.Collection {
	return b.chunksColl
}

func (b *Bucket) openDownloadStream(filter interface{}, opts ...*options.FindOptions) (*DownloadStream, error) {
	ctx, cancel := deadlineContext(b.readDeadline)
	if cancel != nil {
		defer cancel()
	}

	cursor, err := b.findFile(ctx, filter, opts...)
	if err != nil {
		return nil, err
	}

	// Unmarshal the data into a File instance, which can be passed to newDownloadStream. The _id value has to be
	// parsed out separately because "_id" will not match the File.ID field and we want to avoid exposing BSON tags
	// in the File type. After parsing it, use RawValue.Unmarshal to ensure File.ID is set to the appropriate value.
	var foundFile File
	if err = cursor.Decode(&foundFile); err != nil {
		return nil, fmt.Errorf("error decoding files collection document: %w", err)
	}

	if foundFile.Length == 0 {
		return newDownloadStream(nil, foundFile.ChunkSize, &foundFile), nil
	}

	// For a file with non-zero length, chunkSize must exist so we know what size to expect when downloading chunks.
	if _, err := cursor.Current.LookupErr("chunkSize"); err != nil {
		return nil, ErrMissingChunkSize
	}

	chunksCursor, err := b.findChunks(ctx, foundFile.ID)
	if err != nil {
		return nil, err
	}
	// The chunk size can be overridden for individual files, so the expected chunk size should be the "chunkSize"
	// field from the files collection document, not the bucket's chunk size.
	return newDownloadStream(chunksCursor, foundFile.ChunkSize, &foundFile), nil
}

func deadlineContext(deadline time.Time) (context.Context, context.CancelFunc) {
	if deadline.Equal(time.Time{}) {
		return context.Background(), nil
	}

	return context.WithDeadline(context.Background(), deadline)
}

func (b *Bucket) downloadToStream(ds *DownloadStream, stream io.Writer) (int64, error) {
	err := ds.SetReadDeadline(b.readDeadline)
	if err != nil {
		_ = ds.Close()
		return 0, err
	}

	copied, err := io.Copy(stream, ds)
	if err != nil {
		_ = ds.Close()
		return 0, err
	}

	return copied, ds.Close()
}

func (b *Bucket) deleteChunks(ctx context.Context, fileID interface{}) error {
	_, err := b.chunksColl.DeleteMany(ctx, bson.D{{"files_id", fileID}})
	return err
}

func (b *Bucket) findFile(ctx context.Context, filter interface{}, opts ...*options.FindOptions) (*mongo.Cursor, error) {
	cursor, err := b.filesColl.Find(ctx, filter, opts...)
	if err != nil {
		return nil, err
	}

	if !cursor.Next(ctx) {
		_ = cursor.Close(ctx)
		return nil, ErrFileNotFound
	}

	return cursor, nil
}

func (b *Bucket) findChunks(ctx context.Context, fileID interface{}) (*mongo.Cursor, error) {
	chunksCursor, err := b.chunksColl.Find(ctx,
		bson.D{{"files_id", fileID}},
		options.Find().SetSort(bson.D{{"n", 1}})) // sort by chunk index
	if err != nil {
		return nil, err
	}

	return chunksCursor, nil
}

// returns true if the 2 index documents are equal
func numericalIndexDocsEqual(expected, actual bsoncore.Document) (bool, error) {
	if bytes.Equal(expected, actual) {
		return true, nil
	}

	actualElems, err := actual.Elements()
	if err != nil {
		return false, err
	}
	expectedElems, err := expected.Elements()
	if err != nil {
		return false, err
	}

	if len(actualElems) != len(expectedElems) {
		return false, nil
	}

	for idx, expectedElem := range expectedElems {
		actualElem := actualElems[idx]
		if actualElem.Key() != expectedElem.Key() {
			return false, nil
		}

		actualVal := actualElem.Value()
		expectedVal := expectedElem.Value()
		actualInt, actualOK := actualVal.AsInt64OK()
		expectedInt, expectedOK := expectedVal.AsInt64OK()

		//GridFS indexes always have numeric values
		if !actualOK || !expectedOK {
			return false, nil
		}

		if actualInt != expectedInt {
			return false, nil
		}
	}
	return true, nil
}

// Create an index if it doesn't already exist
func createNumericalIndexIfNotExists(ctx context.Context, iv mongo.IndexView, model mongo.IndexModel) error {
	c, err := iv.List(ctx)
	if err != nil {
		return err
	}
	defer func() {
		_ = c.Close(ctx)
	}()

	modelKeysBytes, err := bson.Marshal(model.Keys)
	if err != nil {
		return err
	}
	modelKeysDoc := bsoncore.Document(modelKeysBytes)

	for c.Next(ctx) {
		keyElem, err := c.Current.LookupErr("key")
		if err != nil {
			return err
		}

		keyElemDoc := keyElem.Document()

		found, err := numericalIndexDocsEqual(modelKeysDoc, bsoncore.Document(keyElemDoc))
		if err != nil {
			return err
		}
		if found {
			return nil
		}
	}

	_, err = iv.CreateOne(ctx, model)
	return err
}

// create indexes on the files and chunks collection if needed
func (b *Bucket) createIndexes(ctx context.Context) error {
	// must use primary read pref mode to check if files coll empty
	cloned, err := b.filesColl.Clone(options.Collection().SetReadPreference(readpref.Primary()))
	if err != nil {
		return err
	}

	docRes := cloned.FindOne(ctx, bson.D{}, options.FindOne().SetProjection(bson.D{{"_id", 1}}))

	_, err = docRes.Raw()
	if !errors.Is(err, mongo.ErrNoDocuments) {
		// nil, or error that occurred during the FindOne operation
		return err
	}

	filesIv := b.filesColl.Indexes()
	chunksIv := b.chunksColl.Indexes()

	filesModel := mongo.IndexModel{
		Keys: bson.D{
			{"filename", int32(1)},
			{"uploadDate", int32(1)},
		},
	}

	chunksModel := mongo.IndexModel{
		Keys: bson.D{
			{"files_id", int32(1)},
			{"n", int32(1)},
		},
		Options: options.Index().SetUnique(true),
	}

	if err = createNumericalIndexIfNotExists(ctx, filesIv, filesModel); err != nil {
		return err
	}
	return createNumericalIndexIfNotExists(ctx, chunksIv, chunksModel)
}

func (b *Bucket) checkFirstWrite(ctx context.Context) error {
	if !b.firstWriteDone {
		// before the first write operation, must determine if files collection is empty
		// if so, create indexes if they do not already exist

		if err := b.createIndexes(ctx); err != nil {
			return err
		}
		b.firstWriteDone = true
	}

	return nil
}

func (b *Bucket) parseUploadOptions(opts ...*options.UploadOptions) (*Upload, error) {
	upload := &Upload{
		chunkSize: b.chunkSize, // upload chunk size defaults to bucket's value
	}

	uo := options.MergeUploadOptions(opts...)
	if uo.ChunkSizeBytes != nil {
		upload.chunkSize = *uo.ChunkSizeBytes
	}
	if uo.Registry == nil {
		uo.Registry = bson.DefaultRegistry
	}
	if uo.Metadata != nil {
		// TODO(GODRIVER-2726): Replace with marshal() and unmarshal() once the
		// TODO gridfs package is merged into the mongo package.
		raw, err := bson.MarshalWithRegistry(uo.Registry, uo.Metadata)
		if err != nil {
			return nil, err
		}
		var doc bson.D
		unMarErr := bson.UnmarshalWithRegistry(uo.Registry, raw, &doc)
		if unMarErr != nil {
			return nil, unMarErr
		}
		upload.metadata = doc
	}

	return upload, nil
}
