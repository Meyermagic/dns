// EDNS0
//
// EDNS0 is an extension mechanism for the DNS defined in RFC 2671. It defines a 
// standard RR type, the OPT RR, which is then completely abused. 
// Basic use pattern for creating an (empty) OPT RR:
//
//	o := new(dns.OPT)
//	o.Hdr.Name = "." // MUST be the root zone, per definition.
//	o.Hdr.Rrtype = dns.TypeOPT
//
// The rdata of an OPT RR consists out of a slice of EDNS0 interfaces. Currently
// only a few have been standardized: EDNS0_NSID (RFC 5001) and EDNS0_SUBNET (draft). Note that
// these options may be combined in an OPT RR.
// Basic use pattern for a server to check if (and which) options are set:
//
//	// o is a dns.OPT
//	for _, s := range o.Options {
//		switch e := s.(type) {
//		case *dns.EDNS0_NSID:
//			// do stuff with e.Nsid
//		case *dns.EDNS0_SUBNET:
//			// access e.Family, e.Address, etc.
//		}
//	}
package dns

import (
	"encoding/hex"
	"errors"
	"net"
	"strconv"
)

// EDNS0 Option codes.
const (
	EDNS0LLQ         = 0x1    // in progress
	EDNS0UL          = 0x2    // (not used) alias for EDNS0UPDATELEASE
	EDNS0UPDATELEASE = 0x2    // update lease draft
	EDNS0NSID        = 0x3    // nsid (RFC5001)
	EDNS0SUBNET      = 0x50fa // client-subnet draft
	_DO              = 1 << 7 // dnssec ok
)

type OPT struct {
	Hdr    RR_Header
	Option []EDNS0 `dns:"opt"`
}

func (rr *OPT) Header() *RR_Header {
	return &rr.Hdr
}

func (rr *OPT) String() string {
	s := "\n;; OPT PSEUDOSECTION:\n; EDNS: version " + strconv.Itoa(int(rr.Version())) + "; "
	if rr.Do() {
		s += "flags: do; "
	} else {
		s += "flags: ; "
	}
	s += "udp: " + strconv.Itoa(int(rr.UDPSize()))

	for _, o := range rr.Option {
		switch o.(type) {
		case *EDNS0_NSID:
			s += "\n; NSID: " + o.String()
			h, e := o.pack()
			var r string
			if e == nil {
				for _, c := range h {
					r += "(" + string(c) + ")"
				}
				s += "  " + r
			}
		case *EDNS0_SUBNET:
			s += "\n; SUBNET: " + o.String()
		case *EDNS0_UPDATE_LEASE:
			s += "\n; LEASE: " + o.String()
		case *EDNS0_LLQ:
			s += "\n; LLQ: " + o.String()
		}
	}
	return s
}

func (rr *OPT) Len() int {
	l := rr.Hdr.Len()
	for i := 0; i < len(rr.Option); i++ {
		lo, _ := rr.Option[i].pack()
		l += 2 + len(lo)
	}
	return l
}

func (rr *OPT) Copy() RR {
	return &OPT{*rr.Hdr.CopyHeader(), rr.Option}
}

// Version returns the EDNS version used. Only zero is defined.
func (rr *OPT) Version() uint8 {
	return uint8(rr.Hdr.Ttl & 0x00FF00FFFF)
}

// SetVersion sets the version of EDNS. This is usually zero.
func (rr *OPT) SetVersion(v uint8) {
	rr.Hdr.Ttl = rr.Hdr.Ttl&0xFF00FFFF | uint32(v)
}

// UDPSize returns the UDP buffer size.
func (rr *OPT) UDPSize() uint16 {
	return rr.Hdr.Class
}

// SetUDPSize sets the UDP buffer size.
func (rr *OPT) SetUDPSize(size uint16) {
	rr.Hdr.Class = size
}

// Do returns the value of the DO (DNSSEC OK) bit.
func (rr *OPT) Do() bool {
	return byte(rr.Hdr.Ttl>>8)&_DO == _DO
}

// SetDo sets the DO (DNSSEC OK) bit.
func (rr *OPT) SetDo() {
	b1 := byte(rr.Hdr.Ttl >> 24)
	b2 := byte(rr.Hdr.Ttl >> 16)
	b3 := byte(rr.Hdr.Ttl >> 8)
	b4 := byte(rr.Hdr.Ttl)
	b3 |= _DO // Set it
	rr.Hdr.Ttl = uint32(b1)<<24 | uint32(b2)<<16 | uint32(b3)<<8 | uint32(b4)
}

// EDNS0 defines an EDNS0 Option. An OPT RR can have multiple options appended to
// it. Basic use pattern for adding an option to and OPT RR:
//
//	// o is the OPT RR, e is the EDNS0 option
//	o.Option = append(o.Option, e)
type EDNS0 interface {
	// Option returns the option code for the option.
	Option() uint16
	// pack returns the bytes of the option data.
	pack() ([]byte, error)
	// unpack sets the data as found in the buffer. Is also sets
	// the length of the slice as the length of the option data.
	unpack([]byte)
	// String returns the string representation of the option.
	String() string
}

// The nsid EDNS0 option is used to retrieve some sort of nameserver
// identifier. When seding a request Nsid must be set to the empty string
// The identifier is an opaque string encoded as hex.
// Basic use pattern for creating an nsid option:
//
//	o := new(dns.OPT)
//	o.Hdr.Name = "."
//	o.Hdr.Rrtype = dns.TypeOPT
//	e := new(dns.EDNS0_NSID)
//	e.Code = dns.EDNS0NSID
//	o.Option = append(o.Option, e)
type EDNS0_NSID struct {
	Code uint16 // Always EDNS0NSID
	Nsid string // This string needs to be hex encoded
}

func (e *EDNS0_NSID) Option() uint16 {
	return EDNS0NSID
}

func (e *EDNS0_NSID) pack() ([]byte, error) {
	h, err := hex.DecodeString(e.Nsid)
	if err != nil {
		return nil, err
	}
	return h, nil
}

func (e *EDNS0_NSID) unpack(b []byte) {
	e.Nsid = hex.EncodeToString(b)
}

func (e *EDNS0_NSID) String() string {
	return string(e.Nsid)
}

// The subnet EDNS0 option is used to give the remote nameserver
// an idea of where the client lives. It can then give back a different
// answer depending on the location or network topology.
// Basic use pattern for creating an subnet option:
//
//	o := new(dns.OPT)
//	o.Hdr.Name = "."
//	o.Hdr.Rrtype = dns.TypeOPT
//	e := new(dns.EDNS0_SUBNET)
//	e.Code = dns.EDNS0SUBNET
//	e.Family = 1	// 1 for IPv4 source address, 2 for IPv6
//	e.NetMask = 32	// 32 for IPV4, 128 for IPv6
//	e.SourceScope = 0
//	e.Address = net.ParseIP("127.0.0.1").To4()	// for IPv4
//	// e.Address = net.ParseIP("2001:7b8:32a::2")	// for IPV6
//	o.Option = append(o.Option, e)
type EDNS0_SUBNET struct {
	Code          uint16 // Always EDNS0SUBNET
	Family        uint16 // 1 for IP, 2 for IP6
	SourceNetmask uint8
	SourceScope   uint8
	Address       net.IP
}

func (e *EDNS0_SUBNET) Option() uint16 {
	return EDNS0SUBNET
}

func (e *EDNS0_SUBNET) pack() ([]byte, error) {
	b := make([]byte, 4)
	b[0], b[1] = packUint16(e.Family)
	b[2] = e.SourceNetmask
	b[3] = e.SourceScope
	switch e.Family {
	case 1:
		if e.SourceNetmask > net.IPv4len*8 {
			return nil, errors.New("bad netmask")
		}
		ip := make([]byte, net.IPv4len)
		a := e.Address.To4().Mask(net.CIDRMask(int(e.SourceNetmask), net.IPv4len*8))
		for i := 0; i < net.IPv4len; i++ {
			if i+1 > len(e.Address) {
				break
			}
			ip[i] = a[i]
		}
		b = append(b, ip...)
	case 2:
		if e.SourceNetmask > net.IPv6len*8 {
			return nil, errors.New("bad netmask")
		}
		ip := make([]byte, net.IPv6len)
		a := e.Address.Mask(net.CIDRMask(int(e.SourceNetmask), net.IPv6len*8))
		for i := 0; i < net.IPv6len; i++ {
			if i+1 > len(e.Address) {
				break
			}
			ip[i] = a[i]
		}
		b = append(b, ip...)
	default:
		return nil, errors.New("bad address family")
	}
	return b, nil
}

func (e *EDNS0_SUBNET) unpack(b []byte) {
	if len(b) < 8 {
		return
	}
	e.Family, _ = unpackUint16(b, 0)
	e.SourceNetmask = b[2]
	e.SourceScope = b[3]
	switch e.Family {
	case 1:
		if len(b) == 8 {
			e.Address = net.IPv4(b[4], b[5], b[6], b[7])
		}
	case 2:
		if len(b) == 20 {
			e.Address = net.IP{b[4], b[4+1], b[4+2], b[4+3], b[4+4],
				b[4+5], b[4+6], b[4+7], b[4+8], b[4+9], b[4+10],
				b[4+11], b[4+12], b[4+13], b[4+14], b[4+15]}
		}
	}
	return
}

func (e *EDNS0_SUBNET) String() (s string) {
	if e.Address == nil {
		s = "<nil>"
	} else if e.Address.To4() != nil {
		s = e.Address.String()
	} else {
		s = "[" + e.Address.String() + "]"
	}
	s += "/" + strconv.Itoa(int(e.SourceNetmask)) + "/" + strconv.Itoa(int(e.SourceScope))
	return
}

// The UPDATE_LEASE EDNS0 (draft RFC) option is used to tell the server to set
// an expiration on an update RR. This is helpful for clients that cannot clean
// up after themselves. This is a draft RFC and more information can be found at
// http://files.dns-sd.org/draft-sekar-dns-ul.txt 
//
//	o := new(dns.OPT)
//	o.Hdr.Name = "."
//	o.Hdr.Rrtype = dns.TypeOPT
//	e := new(dns.EDNS0_UPDATE_LEASE)
//	e.Code = dns.EDNS0UPDATELEASE
//	e.Lease = 120 // in seconds
//	o.Option = append(o.Option, e)

type EDNS0_UPDATE_LEASE struct {
	Code  uint16 // Always EDNS0UPDATELEASE
	Lease uint32
}

func (e *EDNS0_UPDATE_LEASE) Option() uint16 {
	return EDNS0UPDATELEASE
}

// Copied: http://golang.org/src/pkg/net/dnsmsg.go
func (e *EDNS0_UPDATE_LEASE) pack() ([]byte, error) {
	b := make([]byte, 4)
	b[0] = byte(e.Lease >> 24)
	b[1] = byte(e.Lease >> 16)
	b[2] = byte(e.Lease >> 8)
	b[3] = byte(e.Lease)
	return b, nil
}

func (e *EDNS0_UPDATE_LEASE) unpack(b []byte) {
	e.Lease = uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func (e *EDNS0_UPDATE_LEASE) String() string {
	return strconv.Itoa(int(e.Lease))
}

//TODO: Actually EDNS0LLQ
// The UPDATE_LEASE EDNS0 (draft RFC) option is used to tell the server to set
// an expiration on an update RR. This is helpful for clients that cannot clean
// up after themselves. This is a draft RFC and more information can be found at
// http://files.dns-sd.org/draft-sekar-dns-ul.txt 
//
//	o := new(dns.RR_OPT)
//	o.Hdr.Name = "."
//	o.Hdr.Rrtype = dns.TypeOPT
//	e := new(dns.EDNS0_UPDATE_LEASE)
//	e.Code = dns.EDNS0UPDATELEASE
//	e.Lease = 120 // in seconds
//	o.Option = append(o.Option, e)

type EDNS0_LLQ struct {
	Code uint16 // Always EDNS0LLQ
	//TODO: Spec says the whole (Length, code, fields) section can repeat
	//Is this part of the normal multiple RR_Opt thing, or specific to LLQ?
	//If LLQ-specific, I'll need to re-write this DNS Libary's handling of 
	//EDNS0 packets, to allow us to unpack based on the length, too
	Version   uint16
	LLQOpcode uint16
	ErrorCode uint16
	LLQID     uint64
	LeaseLife uint32
}

func (e *EDNS0_LLQ) Option() uint16 {
	return EDNS0LLQ
}

// Copied: http://golang.org/src/pkg/net/dnsmsg.go
func (e *EDNS0_LLQ) pack() ([]byte, error) {
	b := make([]byte, 18)
	b[0] = byte(e.Version >> 8)
	b[1] = byte(e.Version)
	b[2] = byte(e.LLQOpcode >> 8)
	b[3] = byte(e.LLQOpcode)
	b[4] = byte(e.ErrorCode >> 8)
	b[5] = byte(e.ErrorCode)
	b[6] = byte(e.LLQID >> 56)
	b[7] = byte(e.LLQID >> 48)
	b[8] = byte(e.LLQID >> 40)
	b[9] = byte(e.LLQID >> 32)
	b[10] = byte(e.LLQID >> 24)
	b[11] = byte(e.LLQID >> 16)
	b[12] = byte(e.LLQID >> 8)
	b[13] = byte(e.LLQID)
	b[14] = byte(e.LeaseLife >> 24)
	b[15] = byte(e.LeaseLife >> 16)
	b[16] = byte(e.LeaseLife >> 8)
	b[17] = byte(e.LeaseLife)
	return b, nil
}

func (e *EDNS0_LLQ) unpack(b []byte) {
	e.Version = uint16(b[0])<<8 | uint16(b[1])
	e.LLQOpcode = uint16(b[2])<<8 | uint16(b[3])
	e.ErrorCode = uint16(b[4])<<8 | uint16(b[5])
	e.LLQID = uint64(b[6])<<56 | uint64(b[7])<<48 | uint64(b[8])<<40 | uint64(b[9])<<32 | uint64(b[10])<<24 | uint64(b[11])<<16 | uint64(b[12])<<8 | uint64(b[13])
	e.LeaseLife = uint32(b[14])<<24 | uint32(b[15])<<16 | uint32(b[16])<<8 | uint32(b[17])
}

func (e *EDNS0_LLQ) String() string {
	return "(" + strconv.FormatUint(uint64(e.Version), 10) + " " +
		strconv.FormatUint(uint64(e.LLQOpcode), 10) + " " +
		strconv.FormatUint(uint64(e.ErrorCode), 10) + " " +
		strconv.FormatUint(e.LLQID, 10) + " " +
		strconv.FormatUint(uint64(e.LeaseLife), 10) + ")"
}
