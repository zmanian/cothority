package main

import (
	"bytes"
	"errors"
	"github.com/dedis/crypto/abstract"
	"github.com/dedis/crypto/config"
	"github.com/dedis/crypto/poly"
	"github.com/dedis/crypto/random"
	"github.com/dedis/crypto/sig"
	//"log"
)

// XXX should be config items
const thresT = 2
const thresR = 2
const thresN = 3

type Server struct {

	// Network interface
	host Host

	// XXX use more generic pub/private keypair infrastructure
	// to support PGP, SSH, etc keys?
	suite   abstract.Suite
	rand    abstract.Cipher
	keysize int
	keypair config.KeyPair

	// XXX servers shouldn't really need to know everyone else
	conn   Conn
	clipub sig.PublicKey // client's public key
	srvpub []sig.SchnorrPublicKey
	srvsec sig.SchnorrSecretKey
	self   int // our server index
}

func (s *Server) init(host Host, suite abstract.Suite,
	clipub sig.PublicKey, srvpub []sig.SchnorrPublicKey,
	srvsec sig.SchnorrSecretKey, self int) {
	s.host = host
	s.suite = suite
	s.rand = suite.Cipher(abstract.RandomKey)
	s.keysize = s.rand.KeySize()
	s.clipub = clipub
	s.srvpub = srvpub
	s.srvsec = srvsec
	s.keypair = config.KeyPair{suite, srvsec.Point, srvsec.Secret} // XXX
	s.self = self
}

func (s *Server) serve(conn Conn) (err error) {
	s.conn = conn

	// Receive client's I1
	var i1 I1
	if err = s.recv(&i1); err != nil {
		return
	}

	// Choose server's trustee-selection randomness
	Rs := make([]byte, s.keysize)
	s.rand.XORKeyStream(Rs, Rs)

	// Send our R1
	var r1 R1
	r1.HRs = abstract.Sum(s.suite, Rs)
	err = s.send(&r1)
	if err != nil {
		return err
	}

	// Receive client's I2
	var i2 I2
	if err = s.recv(&i2); err != nil {
		return
	}
	Rc := i2.Rc
	HRc := abstract.Sum(s.suite, Rc)
	if !bytes.Equal(HRc, i1.HRc) {
		return errors.New("client random hash mismatch")
	}

	// Construct our Deal
	secPair := &config.KeyPair{}
	secPair.Gen(s.suite, random.Stream)
	sel := pickInsurers(s.suite, s.srvpub, Rc, Rs)
	selkeys := make([]abstract.Point, len(sel))
	for i := range sel {
		selkeys[i] = s.srvpub[sel[i]].Point
	}
	deal := &poly.Promise{}
	deal.ConstructPromise(secPair, &s.keypair, thresT, thresR, selkeys)
	dealb, err := deal.MarshalBinary()
	if err != nil {
		return
	}

	// Send our R2
	r2 := R2{Rs: Rs, Deal: dealb}
	err = s.send(&r2)
	if err != nil {
		return
	}

	// Receive client's I3
	var i3 I3
	if err = s.recv(&i3); err != nil {
		return
	}

	// Decrypt and validate all the shares we've been dealt.
	nsrv := len(s.srvpub)
	if len(i3.R2s) != nsrv {
		return errors.New("wrong-length R2 array in I3 message")
	}
	shares := []R4Share{}
	r3resps := []R3Resp{}
	for i := 0; i < nsrv; i++ {
		r2i := R2{}
		r2ib := i3.R2s[i]
		if len(r2ib) == 0 {
			continue // Missing R2 - that's OK, just skip
		}
		if err = sigDecode(s.suite, &s.srvpub[i], r2ib, &r2i); err != nil {
			return
		}
		// XXX equivocation-check other servers' responses

		// Unmarshal and validate server i's Deal
		deal := &poly.Promise{}
		deal.UnmarshalInit(thresT, thresR, thresN, s.suite)
		if err = deal.UnmarshalBinary(r2i.Deal); err != nil {
			return
		}

		// Which insurers did server i deal its secret to?
		sel := pickInsurers(s.suite, s.srvpub, Rc, r2i.Rs)
		for k := range sel {
			if sel[k] != s.self {
				continue // share dealt to someone else
			}

			// Decrypt and validate the specific share we were dealt
			// XXX produce response rather than returning if invalid
			share, resp, err := deal.ProduceResponse(
				k, &s.keypair)
			if err != nil {
				return err
			}

			// Marshal the response to return to the client
			var r3resp R3Resp
			r3resp.Dealer = i
			r3resp.Index = k
			r3resp.Resp, err = resp.MarshalBinary()
			if err != nil {
				return err
			}
			r3resps = append(r3resps, r3resp)

			// Save the revealed share for later
			shares = append(shares, R4Share{i, k, share})

			//log.Printf("server %d dealt server %d share %d",
			//	i, s.self, k)
		}
	}

	// Send our R3
	r3 := R3{Resp: r3resps}
	err = s.send(&r3)
	if err != nil {
		return err
	}

	// Receive client's I4
	var i4 I4
	if err = s.recv(&i4); err != nil {
		return
	}

	// Validate the R4, mainly just making sure it's a subset of the R3 set
	if len(i4.R2s) != nsrv {
		return errors.New("wrong-length R2 array in I4 message")
	}
	for i := 0; i < nsrv; i++ {
		r2ib := i4.R2s[i]
		if len(r2ib) != 0 && !bytes.Equal(r2ib, i3.R2s[i]) {
			return errors.New("R2 set in I4 not a subset of I3")
		}
	}

	// Send our R4
	// XXX but only if our deal is still included?
	r4 := R4{Shares: shares}
	err = s.send(&r4)
	if err != nil {
		return
	}

	return
}

func (s *Server) recv(obj interface{}) (err error) {

	// Receive the client's next message
	var msg []byte
	if msg, err = s.conn.Recv(); err != nil {
		return
	}

	// Decode the message and verify its signature
	if err = sigDecode(s.suite, s.clipub, msg, obj); err != nil {
		return
	}

	return
}

func (s *Server) send(obj interface{}) (err error) {

	var msg []byte
	if msg, err = sigEncode(s.suite, &s.srvsec, s.rand, obj); err != nil {
		return
	}

	if err = s.conn.Send(msg); err != nil {
		return
	}

	return
}
