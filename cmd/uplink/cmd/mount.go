// Copyright (C) 2018 Storj Labs, Inc.
// See LICENSE for copying information.

// +build linux darwin netbsd freebsd openbsd

package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/go-fuse/fuse/nodefs"
	"github.com/hanwen/go-fuse/fuse/pathfs"
	"github.com/spf13/cobra"

	"storj.io/storj/pkg/paths"
	"storj.io/storj/pkg/process"
	"storj.io/storj/pkg/storage/meta"
	"storj.io/storj/pkg/storage/objects"
	"storj.io/storj/pkg/utils"
	"storj.io/storj/storage/boltdb"
)

func init() {
	addCmd(&cobra.Command{
		Use:   "mount",
		Short: "Mount a bucket",
		RunE:  mountBucket,
	})
}

func mountBucket(cmd *cobra.Command, args []string) (err error) {
	if len(args) == 0 {
		return fmt.Errorf("No bucket specified for mounting")
	}
	if len(args) == 1 {
		return fmt.Errorf("No destination specified")
	}

	ctx := process.Ctx(cmd)

	bs, err := cfg.BucketStore(ctx)
	if err != nil {
		return err
	}

	u, err := utils.ParseURL(args[0])
	if err != nil {
		return err
	}
	if u.Host == "" {
		return fmt.Errorf("No bucket specified. Please use format sj://bucket/")
	}

	store, err := bs.GetObjectStore(ctx, u.Host)
	if err != nil {
		return err
	}

	nfs := pathfs.NewPathNodeFs(&storjFs{FileSystem: pathfs.NewDefaultFileSystem(), ctx: ctx, store: store}, nil)
	server, _, err := nodefs.MountRoot(args[1], nfs.Root(), nil)
	if err != nil {
		return fmt.Errorf("Mount failed: %v", err)
	}

	// detect control-c and unmount
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		for range sig {
			if err := server.Unmount(); err != nil {
				fmt.Printf("Unmount failed: %v", err)
			}
		}
	}()

	server.Serve()
	return nil
}

type storjFs struct {
	ctx   context.Context
	store objects.Store
	pathfs.FileSystem
}

func (sf *storjFs) GetAttr(name string, context *fuse.Context) (*fuse.Attr, fuse.Status) {
	if name == "" {
		return &fuse.Attr{Mode: fuse.S_IFDIR | 0755}, fuse.OK
	}

	meta, err := sf.store.Meta(sf.ctx, paths.New(name))
	if err != nil {
		if boltdb.ErrKeyNotFound.Has(err) {
			fmt.Println(err)
		}
		return nil, fuse.ENOENT
	}

	return &fuse.Attr{
		Owner: *fuse.CurrentOwner(),
		Mode:  fuse.S_IFREG | 0644,
		Size:  uint64(meta.Size),
		Mtime: uint64(meta.Modified.Unix()),
	}, fuse.OK
}

func (sf *storjFs) OpenDir(name string, context *fuse.Context) (c []fuse.DirEntry, code fuse.Status) {
	if name == "" {
		entries, err := sf.listFiles(sf.ctx, sf.store)
		if err != nil {
			fmt.Println(err)
			return nil, fuse.EIO
		}

		return entries, fuse.OK
	}
	return nil, fuse.ENOENT
}

func (sf *storjFs) Open(name string, flags uint32, context *fuse.Context) (file nodefs.File, code fuse.Status) {
	return &storjFile{
		name:  name,
		ctx:   sf.ctx,
		store: sf.store,
		File:  nodefs.NewDefaultFile(),
	}, fuse.OK
}

func (sf *storjFs) listFiles(ctx context.Context, store objects.Store) (c []fuse.DirEntry, err error) {
	var entries []fuse.DirEntry

	startAfter := paths.New("")

	for {
		items, more, err := store.List(ctx, paths.New(""), startAfter, nil, false, 0, meta.Modified)
		if err != nil {
			return nil, err
		}

		for _, object := range items {
			path := object.Path.String()

			var d fuse.DirEntry
			d.Name = path
			d.Mode = fuse.S_IFREG
			entries = append(entries, fuse.DirEntry(d))
		}

		if !more {
			break
		}

		startAfter = items[len(items)-1].Path
	}

	return entries, nil
}

type storjFile struct {
	name  string
	ctx   context.Context
	store objects.Store
	nodefs.File
}

func (f *storjFile) Read(buf []byte, off int64) (res fuse.ReadResult, code fuse.Status) {
	rr, _, err := f.store.Get(f.ctx, paths.New(f.name))
	if err != nil {
		fmt.Println(err)
		return nil, fuse.EIO
	}
	defer utils.LogClose(rr)

	length := int64(len(buf))
	if rr.Size()-off < length {
		length = rr.Size() - off
	}
	r, err := rr.Range(f.ctx, off, length)
	if err != nil {
		fmt.Println(err)
		return nil, fuse.EIO
	}
	defer utils.LogClose(r)

	var n int
	for {
		nn, err := r.Read(buf[n:])
		if err != nil {
			if err != io.EOF {
				fmt.Println(err)
				return nil, fuse.EIO
			}
			break
		}
		n += nn
	}

	return fuse.ReadResultData(buf[:n]), fuse.OK
}
