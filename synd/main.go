package main

import (
	"flag"
	"fmt"
	"os"
	"sort"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/gholt/ring"
	pb "github.com/pandemicsyn/syndicate/api/proto"

	"log"
	"net"
	"path/filepath"
	"strings"
)

var (
	printVersionInfo = flag.Bool("version", false, "print version/build info")
)

var syndVersion string
var ringVersion string
var goVersion string
var buildDate string

// FatalIf is just a lazy log/panic on error func
func FatalIf(err error, msg string) {
	if err != nil {
		log.Fatalf("%s: %v", msg, err)
	}
}

func Filter(vs []string, f func(string) bool) []string {
	vsf := make([]string, 0)
	for _, v := range vs {
		if f(v) {
			vsf = append(vsf, v)
		}
	}
	return vsf
}

func getRingPaths(cfg *Config) (lastBuilder string, lastRing string, err error) {
	_, err = os.Stat(filepath.Join(cfg.RingDir, "oort.builder"))
	if err != nil {
		//TODO: no active builder found, so should we search for the most recent one
		//we can find and load it and hopefully its matching ring?
		return "", "", fmt.Errorf("No builder file found in %s", cfg.RingDir)
	}
	lastBuilder = filepath.Join(cfg.RingDir, "oort.builder")
	_, err = os.Stat(filepath.Join(cfg.RingDir, "oort.ring"))
	if err != nil {
		//TODO: if we don't find a matching oort.ring should we just
		// use oort.builder to make new one ?
		return "", "", fmt.Errorf("No ring file found in %s", cfg.RingDir)
	}
	lastRing = filepath.Join(cfg.RingDir, "oort.ring")
	return lastBuilder, lastRing, nil
}

func findLastRing(cfg *Config) (lastBuilder string, lastRing string, err error) {
	fp, err := os.Open(cfg.RingDir)
	if err != nil {
		return "", "", err
	}
	names, err := fp.Readdirnames(-1)
	fp.Close()
	if err != nil {
		return "", "", err
	}

	fn := Filter(names, func(v string) bool {
		return strings.HasSuffix(v, "-oort.builder")
	})
	sort.Strings(fn)
	if len(fn) != 0 {
		lastBuilder = filepath.Join(cfg.RingDir, fn[len(fn)-1])
	}

	fn = Filter(names, func(v string) bool {
		return strings.HasSuffix(v, "-oort.ring")
	})
	if len(fn) != 0 {
		lastRing = filepath.Join(cfg.RingDir, fn[len(fn)-1])
	}
	return lastBuilder, lastRing, nil
}

func newSyndicateServer(cfg *Config) (*ringmgr, error) {
	var err error
	s := new(ringmgr)
	s.cfg = cfg

	bfile, rfile, err := getRingPaths(cfg)
	if err != nil {
		panic(err)
	}
	_, s.b, err = ring.RingOrBuilder(bfile)
	FatalIf(err, fmt.Sprintf("Builder file (%s) load failed:", bfile))
	s.r, _, err = ring.RingOrBuilder(rfile)
	FatalIf(err, fmt.Sprintf("Ring file (%s) load failed:", rfile))
	log.Println("Ring version is:", s.r.Version())
	//TODO: verify ring version in bytes matches what we expect
	s.rb, s.bb, err = s.loadRingBuilderBytes(s.r.Version())
	FatalIf(err, "Attempting to load ring/builder bytes")

	for _, v := range cfg.NetFilter {
		_, n, err := net.ParseCIDR(v)
		if err != nil {
			FatalIf(err, "Invalid network range provided")
		}
		s.netlimits = append(s.netlimits, n)
	}
	s.tierlimits = cfg.TierFilter
	s.managedNodes = bootstrapManagedNodes(s.r)
	s.changeChan = make(chan *changeMsg, 1)
	go s.RingChangeManager()
	s.slaves = cfg.Slaves
	if len(s.slaves) == 0 {
		log.Println("!! Running without slaves, have no one to register !!")
		return s, nil
	}

	failcount := 0
	for _, slave := range s.slaves {
		if err = s.RegisterSlave(slave); err != nil {
			log.Println("Got error:", err)
			failcount++
		}
	}
	if failcount > (len(s.slaves) / 2) {
		log.Fatalln("More than half of the ring slaves failed to respond. Exiting.")
	}
	return s, nil
}

func newRingDistServer() *ringslave {
	s := new(ringslave)
	return s
}

func main() {
	cfg, err := loadConfig("/etc/oort/syndicate.toml")
	if err != nil {
		log.Println(err)
		return
	}
	if *printVersionInfo {
		fmt.Println("syndicate-client:", syndVersion)
		fmt.Println("ring version:", ringVersion)
		fmt.Println("build date:", buildDate)
		fmt.Println("go version:", goVersion)
		return
	}
	if cfg.Master {
		l, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.Port))
		FatalIf(err, "Failed to bind to port")
		var opts []grpc.ServerOption
		if cfg.UseTLS {
			creds, err := credentials.NewServerTLSFromFile(cfg.CertFile, cfg.KeyFile)
			FatalIf(err, "Couldn't load cert from file")
			opts = []grpc.ServerOption{grpc.Creds(creds)}
		}
		s := grpc.NewServer(opts...)

		r, err := newSyndicateServer(cfg)
		FatalIf(err, "Couldn't prep ring mgr server")
		pb.RegisterSyndicateServer(s, r)
		log.Printf("Master starting up on %d...\n", cfg.Port)
		s.Serve(l)
	} else {
		l, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.Port))
		FatalIf(err, "Failed to bind to port")
		var opts []grpc.ServerOption
		if cfg.UseTLS {
			creds, err := credentials.NewServerTLSFromFile(cfg.CertFile, cfg.KeyFile)
			FatalIf(err, "Couldn't load cert from file")
			opts = []grpc.ServerOption{grpc.Creds(creds)}
		}
		s := grpc.NewServer(opts...)

		pb.RegisterRingDistServer(s, newRingDistServer())
		log.Printf("Starting ring slave up on %d...\n", cfg.Port)
		s.Serve(l)
	}
}
