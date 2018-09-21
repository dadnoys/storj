// Copyright (C) 2018 Storj Labs, Inc.
// See LICENSE for copying information.

package server

import (
	"crypto"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"

	"github.com/gtank/cryptopasta"
	"github.com/shirou/gopsutil/disk"
	"github.com/zeebo/errs"
	"golang.org/x/net/context"
	"gopkg.in/spacemonkeygo/monkit.v2"

	"storj.io/storj/pkg/pb"
	"storj.io/storj/pkg/peertls"
	pstore "storj.io/storj/pkg/piecestore"
	"storj.io/storj/pkg/piecestore/rpc/server/psdb"
	"storj.io/storj/pkg/provider"
)

var (
	mon = monkit.Package()

	// ServerError wraps errors returned from Server struct methods
	ServerError = errs.Class("PSServer error")
)

// Config contains everything necessary for a server
type Config struct {
	Path               string `help:"path to store data in" default:"$CONFDIR"`
	AllocatedDiskSpace int64  `help:"total allocated disk space, default(1GB)" default:"1073741824"`
}

// Run implements provider.Responsibility
func (c Config) Run(ctx context.Context, server *provider.Provider) (err error) {
	defer mon.Task()(&ctx)(&err)

	s, err := Initialize(ctx, c, server.Identity().Key)
	if err != nil {
		return err
	}

	pb.RegisterPieceStoreRoutesServer(server.GRPC(), s)

	defer func() {
		log.Fatal(s.Stop(ctx))
	}()

	return server.Run(ctx)
}

//DirSize returns the total size of the files in that directory
func DirSize(path string) (int64, error) {
	var size int64
	_, err := os.Stat(path)
	if err != nil {
		return 0, errors.New("path doesn't exists")
	}
	adjSize := func(_ string, info os.FileInfo, err error) error {
		if !info.IsDir() {
			size += info.Size()
		}
		return err
	}
	err = filepath.Walk(path, adjSize)

	return size, err
}

// Server -- GRPC server meta data used in route calls
type Server struct {
	DataDir        string
	DB             *psdb.DB
	pkey           crypto.PrivateKey
	totalAllocated int64
}

// Initialize -- initializes a server struct
func Initialize(ctx context.Context, config Config, pkey crypto.PrivateKey) (*Server, error) {
	dbPath := filepath.Join(config.Path, "piecestore.db")
	dataDir := filepath.Join(config.Path, "piece-store-data")

	// read the allocated disk space from the config file
	allocatedDiskSpace := config.AllocatedDiskSpace

	// get the disk space details
	diskSpace, err := disk.Usage("/")
	freeDiskSpace := int64(diskSpace.Free)
	if err != nil {
		return nil, err
	}

	db, err := psdb.Open(ctx, dataDir, dbPath)
	if err != nil {
		return nil, err
	}

	// get how much is currently used, if for the first time totalUsed = 0
	totalUsed, err := DirSize(dataDir)
	if err != nil {
		//first time setup
		totalUsed = 0
	}

	// check your hard drive is big enough
	// first time setup as a piece node server
	if (totalUsed == 0x00) && (freeDiskSpace < allocatedDiskSpace) {
		return nil, errors.New("not enough space")
	}

	// on restarting the Piece node server, assuming already been working as a node
	// used above the alloacated space, user changed the allocation space setting
	// before restarting
	if totalUsed >= allocatedDiskSpace {
		fmt.Println("Warning!!! Used more space then allocated")
		log.Println("Warning!!! Used more space then allocated")
		/** [TODO] any special handling needed here ... */
		return &Server{DataDir: dataDir, DB: db, pkey: pkey, totalAllocated: allocatedDiskSpace}, nil
	}

	// the available diskspace is less than remaining allocated space,
	// due to change of setting before restarting
	if freeDiskSpace < (allocatedDiskSpace - totalUsed) {
		log.Println("Warning!!! Disk space is less than remaining allocated space")
		fmt.Println("Warning!!! Disk space is less than remaining allocated space")
		/** [TODO] any special handling needed here ... */
		return &Server{DataDir: dataDir, DB: db, pkey: pkey, totalAllocated: allocatedDiskSpace}, nil
	}

	return nil, nil
}

// Stop the piececstore node
func (s *Server) Stop(ctx context.Context) (err error) {
	return s.DB.Close()
}

// Piece -- Send meta data about a stored by by Id
func (s *Server) Piece(ctx context.Context, in *pb.PieceId) (*pb.PieceSummary, error) {
	log.Printf("Getting Meta for %s...", in.GetId())

	path, err := pstore.PathByID(in.GetId(), s.DataDir)
	if err != nil {
		return nil, err
	}

	match, err := regexp.MatchString("^[A-Za-z0-9]{20,64}$", in.GetId())
	if err != nil {
		return nil, err
	}

	if !match {
		return nil, ServerError.New("Invalid ID")
	}

	fileInfo, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	// Read database to calculate expiration
	ttl, err := s.DB.GetTTLByID(in.GetId())
	if err != nil {
		return nil, err
	}

	log.Printf("Successfully retrieved meta for %s.", in.GetId())
	return &pb.PieceSummary{Id: in.GetId(), Size: fileInfo.Size(), ExpirationUnixSec: ttl}, nil
}

// Stats will return statistics about the Server
func (s *Server) Stats(ctx context.Context, in *pb.StatsReq) (*pb.StatSummary, error) {
	log.Printf("Getting Stats...\n")

	totalUsed, err := s.DB.SumTTLSizes()
	if err != nil {
		return nil, err
	}

	return &pb.StatSummary{UsedSpace: totalUsed, AvailableSpace: (int64(s.totalAllocated) - totalUsed)}, nil
}

// Delete -- Delete data by Id from piecestore
func (s *Server) Delete(ctx context.Context, in *pb.PieceDelete) (*pb.PieceDeleteSummary, error) {
	log.Printf("Deleting %s...", in.GetId())

	if err := s.deleteByID(in.GetId()); err != nil {
		return nil, err
	}

	log.Printf("Successfully deleted %s.", in.GetId())
	return &pb.PieceDeleteSummary{Message: OK}, nil
}

func (s *Server) deleteByID(id string) error {
	if err := pstore.Delete(id, s.DataDir); err != nil {
		return err
	}

	if err := s.DB.DeleteTTLByID(id); err != nil {
		return err
	}

	log.Printf("Deleted data of id (%s) from piecestore\n", id)

	return nil
}

func (s *Server) verifySignature(ctx context.Context, ba *pb.RenterBandwidthAllocation) error {
	// TODO(security): detect replay attacks
	pi, err := provider.PeerIdentityFromContext(ctx)
	if err != nil {
		return err
	}

	k, ok := pi.Leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return peertls.ErrUnsupportedKey.New("%T", pi.Leaf.PublicKey)
	}

	if ok := cryptopasta.Verify(ba.GetData(), ba.GetSignature(), k); !ok {
		return ServerError.New("Failed to verify Signature")
	}
	return nil
}
