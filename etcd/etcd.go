/*
Copyright 2014 CoreOS Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package etcd

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/coreos/etcd/config"
)

const (
	participantMode int64 = iota
	standbyMode
	stopMode
)

type Server struct {
	config       *config.Config
	id           int64
	pubAddr      string
	raftPubAddr  string
	tickDuration time.Duration

	mode atomicInt
	p    *participant
	s    *standby

	client  *v2client
	peerHub *peerHub

	exited      chan error
	stopNotifyc chan struct{}
	log         *log.Logger
	http.Handler
}

func New(c *config.Config) (*Server, error) {
	if err := c.Sanitize(); err != nil {
		log.Fatalf("server.new sanitizeErr=\"%v\"\n", err)
	}

	tc := &tls.Config{
		InsecureSkipVerify: true,
	}
	var err error
	if c.PeerTLSInfo().Scheme() == "https" {
		tc, err = c.PeerTLSInfo().ClientConfig()
		if err != nil {
			log.Fatalf("server.new ClientConfigErr=\"%v\"\n", err)
		}
	}

	tr := new(http.Transport)
	tr.TLSClientConfig = tc
	tr.Dial = (&net.Dialer{Timeout: 200 * time.Millisecond}).Dial
	tr.TLSHandshakeTimeout = 10 * time.Second
	tr.ResponseHeaderTimeout = defaultTickDuration * defaultHeartbeat
	client := &http.Client{Transport: tr}

	s := &Server{
		config:       c,
		id:           genId(),
		pubAddr:      c.Addr,
		raftPubAddr:  c.Peer.Addr,
		tickDuration: defaultTickDuration,

		mode: atomicInt(stopMode),

		client:  newClient(tc),
		peerHub: newPeerHub(client),

		exited:      make(chan error, 1),
		stopNotifyc: make(chan struct{}),
	}
	m := http.NewServeMux()
	m.HandleFunc("/", s.requestHandler)
	m.HandleFunc("/version", versionHandler)
	s.Handler = m

	log.Printf("id=%x server.new raftPubAddr=%s\n", s.id, s.raftPubAddr)
	if err = os.MkdirAll(s.config.DataDir, 0700); err != nil {
		if !os.IsExist(err) {
			return nil, err
		}
	}
	return s, nil
}

func (s *Server) SetTick(tick time.Duration) {
	s.tickDuration = tick
	log.Printf("id=%x server.setTick tick=%q\n", s.id, s.tickDuration)
}

// Stop stops the server elegently.
func (s *Server) Stop() error {
	s.mode.Set(stopMode)
	close(s.stopNotifyc)
	err := <-s.exited
	s.client.CloseConnections()
	s.peerHub.stop()
	log.Printf("id=%x server.stop\n", s.id)
	return err
}

func (s *Server) requestHandler(w http.ResponseWriter, r *http.Request) {
	switch s.mode.Get() {
	case participantMode:
		s.p.ServeHTTP(w, r)
	case standbyMode:
		s.s.ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) RaftHandler() http.Handler {
	return http.HandlerFunc(s.ServeRaftHTTP)
}

func (s *Server) ServeRaftHTTP(w http.ResponseWriter, r *http.Request) {
	switch s.mode.Get() {
	case participantMode:
		s.p.raftHandler().ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) Run() error {
	var d *discoverer
	var seeds []string
	var exit error
	defer func() { s.exited <- exit }()

	durl := s.config.Discovery
	if durl != "" {
		u, err := url.Parse(durl)
		if err != nil {
			exit = err
			return fmt.Errorf("bad discovery URL error: %v", err)
		}
		d = newDiscoverer(u, fmt.Sprint(s.id), s.raftPubAddr)
		if seeds, err = d.discover(); err != nil {
			exit = err
			return err
		}
		log.Printf("id=%x server.run source=-discovery seeds=\"%v\"\n", s.id, seeds)
	} else {
		seeds = s.config.Peers
		log.Printf("id=%x server.run source=-peers seeds=\"%v\"\n", s.id, seeds)
	}
	s.peerHub.setSeeds(seeds)

	next := participantMode
	for {
		switch next {
		case participantMode:
			p, err := newParticipant(s.id, s.pubAddr, s.raftPubAddr, s.config.DataDir, s.client, s.peerHub, s.tickDuration)
			if err != nil {
				log.Printf("id=%x server.run newParicipanteErr=\"%v\"\n", s.id, err)
				exit = err
				return err
			}
			s.p = p
			s.mode.Set(participantMode)
			log.Printf("id=%x server.run mode=participantMode\n", s.id)
			dStopc := make(chan struct{})
			if d != nil {
				go d.heartbeat(dStopc)
			}
			s.p.run(s.stopNotifyc)
			if d != nil {
				close(dStopc)
			}
			next = standbyMode
		case standbyMode:
			s.s = newStandby(s.client, s.peerHub)
			s.mode.Set(standbyMode)
			log.Printf("id=%x server.run mode=standbyMode\n", s.id)
			s.s.run(s.stopNotifyc)
			next = participantMode
		default:
			panic("unsupport mode")
		}
		if s.mode.Get() == stopMode {
			return nil
		}
		s.id = genId()
	}
}

// setId sets the id for the participant. This should only be used for testing.
func (s *Server) setId(id int64) {
	log.Printf("id=%x server.setId oldId=%x\n", id, s.id)
	s.id = id
}
