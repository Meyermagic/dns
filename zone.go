package dns

// A structure for handling zone data

import (
	"fmt"
	"github.com/miekg/radix"
	"math/rand"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Zone represents a DNS zone. It's safe for concurrent use by 
// multilpe goroutines.
type Zone struct {
	Origin       string    // Origin of the zone
	olabels      []string  // origin cut up in labels, just to speed up the isSubDomain method
	Wildcard     int       // Whenever we see a wildcard name, this is incremented
	expired      bool      // Slave zone is expired
	ModTime      time.Time // When is the zone last modified
	*radix.Radix           // Zone data
	*sync.RWMutex
}

type uint16Slice []uint16

func (p uint16Slice) Len() int           { return len(p) }
func (p uint16Slice) Less(i, j int) bool { return p[i] < p[j] }
func (p uint16Slice) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

// SignatureConfig holds the parameters for zone (re)signing. This 
// is copied from OpenDNSSEC. See:
// https://wiki.opendnssec.org/display/DOCS/kasp.xml
type SignatureConfig struct {
	// Validity period of the signatures, typically 2 to 4 weeks.
	Validity time.Duration
	// When the end of the validity approaches, how much time should remain
	// before we start to resign. Typical value is 3 days.
	Refresh time.Duration
	// Jitter is an random amount of time added or subtracted from the 
	// expiration time to ensure not all signatures expire a the same time.
	// Typical value is 12 hours, which means the actual jitter value is
	// between -12..0..+12.
	Jitter time.Duration
	// InceptionOffset is subtracted from the inception time to ensure badly
	// calibrated clocks on the internet can still validate a signature.
	// Typical value is 300 seconds.
	InceptionOffset time.Duration
	// HonorSepFlag is a boolean which when try instructs the signer to use
	// a KSK/ZSK split and only sign the keyset with the KSK(s). If not
	// set all records are signed with all keys. If this flag is true and
	// a single KSK is used for signing, only the keyset is signed.
	HonorSepFlag bool
	// SignerRoutines specifies the number of signing goroutines, if not
	// set runtime.NumCPU() + 1 is used as the value.
	SignerRoutines int
	// SOA Minttl value must be used as the ttl on NSEC/NSEC3 records.
	Minttl uint32
}

func newSignatureConfig() *SignatureConfig {
	return &SignatureConfig{time.Duration(4*7*24) * time.Hour, time.Duration(3*24) * time.Hour, time.Duration(12) * time.Hour, time.Duration(300) * time.Second, true, runtime.NumCPU() + 1, 0}
}

// DefaultSignaturePolicy has the following values. Validity is 4 weeks, 
// Refresh is set to 3 days, Jitter to 12 hours and InceptionOffset to 300 seconds.
// HonorSepFlag is set to true, SignerRoutines is set to runtime.NumCPU() + 1. The
// Minttl value is zero.
var DefaultSignatureConfig = newSignatureConfig()

// NewZone creates an initialized zone with Origin set to origin.
func NewZone(origin string) *Zone {
	if origin == "" {
		origin = "."
	}
	if _, _, ok := IsDomainName(origin); !ok {
		return nil
	}
	z := new(Zone)
	z.Origin = Fqdn(strings.ToLower(origin))
	z.olabels = SplitLabels(z.Origin)
	z.Radix = radix.New()
	z.RWMutex = new(sync.RWMutex)
	z.ModTime = time.Now().UTC()
	return z
}

// ZoneData holds all the RRs having their owner name equal to Name.
type ZoneData struct {
	Name       string              // Domain name for this node
	RR         map[uint16][]RR     // Map of the RR type to the RR
	Signatures map[uint16][]*RRSIG // DNSSEC signatures for the RRs, stored under type covered
	NonAuth    bool                // Always false, except for NSsets that differ from z.Origin
	*sync.RWMutex
}

// NewZoneData creates a new zone data element.
func NewZoneData(s string) *ZoneData {
	zd := new(ZoneData)
	zd.Name = s
	zd.RR = make(map[uint16][]RR)
	zd.Signatures = make(map[uint16][]*RRSIG)
	zd.RWMutex = new(sync.RWMutex)
	return zd
}

// toRadixName reverses a domain name so that when we store it in the radix tree
// we preserve the nsec ordering of the zone (this idea was stolen from NSD).
// Each label is also lowercased.
func toRadixName(d string) string {
	if d == "" || d == "." {
		return "."
	}
	s := ""
	ld := len(d)
	if d[ld-1] != '.' {
		d = d + "."
		ld++
	}
	var lastdot int
	var lastbyte byte
	var lastlastbyte byte
	for i := 0; i < len(d); i++ {
		if d[i] == '.' {
			switch {
			case lastbyte != '\\':
				fallthrough
			case lastbyte == '\\' && lastlastbyte == '\\':
				s = d[lastdot:i] + "." + s
				lastdot = i + 1
				continue
			}
		}
		lastlastbyte = lastbyte
		lastbyte = d[i]
	}
	return "." + strings.ToLower(s[:len(s)-1])
}

// String returns a string representation of a ZoneData. There is no
// String for the entire zone, because this will (most likely) take up
// a huge amount of memory. Basic use pattern for printing an entire
// zone:
//
//	// z contains the zone
//	z.Radix.NextDo(func(i interface{}) {
//		fmt.Printf("%s", i.(*dns.ZoneData).String()) })
//
func (zd *ZoneData) String() string {
	var (
		s string
		t uint16
	)
	// Make sure SOA is first
	// There is only one SOA, but it may have multiple sigs
	if soa, ok := zd.RR[TypeSOA]; ok {
		s += soa[0].String() + "\n"
		if _, ok := zd.Signatures[TypeSOA]; ok {
			for _, sig := range zd.Signatures[TypeSOA] {
				s += sig.String() + "\n"
			}
		}
	}

Types:
	for _, rrset := range zd.RR {
		for _, rr := range rrset {
			t = rr.Header().Rrtype
			if t == TypeSOA || t == TypeNSEC { // Done above or below
				continue Types
			}
			s += rr.String() + "\n"
		}
		if _, ok := zd.Signatures[t]; ok {
			for _, rr := range zd.Signatures[t] {
				s += rr.String() + "\n"
			}
		}
	}
	// Make sure NSEC is last
	// There is only one NSEC, but it may have multiple sigs
	if soa, ok := zd.RR[TypeNSEC]; ok {
		s += soa[0].String() + "\n"
		if _, ok := zd.Signatures[TypeNSEC]; ok {
			for _, sig := range zd.Signatures[TypeNSEC] {
				s += sig.String() + "\n"
			}
		}
	}
	return s
}

// Insert inserts the RR r into the zone. There is no check for duplicate data, although
// Remove will remove all duplicates.
func (z *Zone) Insert(r RR) error {
	if !z.isSubDomain(r.Header().Name) {
		return &Error{Err: "out of zone data", Name: r.Header().Name}
	}

	key := toRadixName(r.Header().Name)
	z.Lock()
	z.ModTime = time.Now().UTC()
	zd, exact := z.Radix.Find(key)
	if !exact {
		// Not an exact match, so insert new value
		defer z.Unlock()
		// Check if it's a wildcard name
		if len(r.Header().Name) > 1 && r.Header().Name[0] == '*' && r.Header().Name[1] == '.' {
			z.Wildcard++
		}
		zd := NewZoneData(r.Header().Name)
		switch t := r.Header().Rrtype; t {
		case TypeRRSIG:
			sigtype := r.(*RRSIG).TypeCovered
			zd.Signatures[sigtype] = append(zd.Signatures[sigtype], r.(*RRSIG))
		case TypeNS:
			// NS records with other names than z.Origin are non-auth
			if r.Header().Name != z.Origin {
				zd.NonAuth = true
			}
			fallthrough
		default:
			zd.RR[t] = append(zd.RR[t], r)
		}
		z.Radix.Insert(key, zd)
		return nil
	}
	z.Unlock()
	zd.Value.(*ZoneData).Lock()
	defer zd.Value.(*ZoneData).Unlock()
	// Name already there
	switch t := r.Header().Rrtype; t {
	case TypeRRSIG:
		sigtype := r.(*RRSIG).TypeCovered
		zd.Value.(*ZoneData).Signatures[sigtype] = append(zd.Value.(*ZoneData).Signatures[sigtype], r.(*RRSIG))
	case TypeNS:
		if r.Header().Name != z.Origin {
			zd.Value.(*ZoneData).NonAuth = true
		}
		fallthrough
	default:
		zd.Value.(*ZoneData).RR[t] = append(zd.Value.(*ZoneData).RR[t], r)
	}
	return nil
}

// Remove removes the RR r from the zone. If the RR can not be found,
// this is a no-op.
func (z *Zone) Remove(r RR) error {
	key := toRadixName(r.Header().Name)
	z.Lock()
	z.ModTime = time.Now().UTC()
	zd, exact := z.Radix.Find(key)
	if !exact {
		defer z.Unlock()
		return nil
	}
	z.Unlock()
	zd.Value.(*ZoneData).Lock()
	defer zd.Value.(*ZoneData).Unlock()
	remove := false
	switch t := r.Header().Rrtype; t {
	case TypeRRSIG:
		sigtype := r.(*RRSIG).TypeCovered
		for i, zr := range zd.Value.(*ZoneData).Signatures[sigtype] {
			if r == zr {
				zd.Value.(*ZoneData).Signatures[sigtype] = append(zd.Value.(*ZoneData).Signatures[sigtype][:i], zd.Value.(*ZoneData).Signatures[sigtype][i+1:]...)
				remove = true
			}
		}
		if remove {
			// If every Signature of the covering type is removed, removed the type from the map
			if len(zd.Value.(*ZoneData).Signatures[sigtype]) == 0 {
				delete(zd.Value.(*ZoneData).Signatures, sigtype)
			}
		}
	default:
		for i, zr := range zd.Value.(*ZoneData).RR[t] {
			// Matching RR
			if r == zr {
				zd.Value.(*ZoneData).RR[t] = append(zd.Value.(*ZoneData).RR[t][:i], zd.Value.(*ZoneData).RR[t][i+1:]...)
				remove = true
			}
		}
		if remove {
			// If every RR of this type is removed, removed the type from the map
			if len(zd.Value.(*ZoneData).RR[t]) == 0 {
				delete(zd.Value.(*ZoneData).RR, t)
			}
		}
	}
	if !remove {
		return nil
	}

	if len(r.Header().Name) > 1 && r.Header().Name[0] == '*' && r.Header().Name[1] == '.' {
		z.Wildcard--
		if z.Wildcard < 0 {
			z.Wildcard = 0
		}
	}
	if len(zd.Value.(*ZoneData).RR) == 0 && len(zd.Value.(*ZoneData).Signatures) == 0 {
		// Entire node is empty, remove it from the Radix tree
		z.Radix.Remove(key)
	}
	return nil
}

// RemoveName removes all the RRs with ownername matching s from the zone. Typical use of this
// method is when processing a RemoveName dynamic update packet.
func (z *Zone) RemoveName(s string) error {
	key := toRadixName(s)
	z.Lock()
	z.ModTime = time.Now().UTC()
	defer z.Unlock()
	z.Radix.Remove(key)
	if len(s) > 1 && s[0] == '*' && s[1] == '.' {
		z.Wildcard--
		if z.Wildcard < 0 {
			z.Wildcard = 0
		}
	}
	return nil
}

// RemoveRRset removes all the RRs with the ownername matching s and the type matching t from the zone.
// Typical use of this method is when processing a RemoveRRset dynamic update packet.
func (z *Zone) RemoveRRset(s string, t uint16) error {
	z.Lock()
	z.ModTime = time.Now().UTC()
	zd, exact := z.Radix.Find(toRadixName(s))
	if !exact {
		defer z.Unlock()
		return nil
	}
	z.Unlock()
	zd.Value.(*ZoneData).Lock()
	defer zd.Value.(*ZoneData).Unlock()
	switch t {
	case TypeRRSIG:
		// empty all signature maps
		for covert, _ := range zd.Value.(*ZoneData).Signatures {
			delete(zd.Value.(*ZoneData).Signatures, covert)
		}
	default:
		// empty all rr maps
		for t, _ := range zd.Value.(*ZoneData).RR {
			delete(zd.Value.(*ZoneData).RR, t)
		}
	}
	return nil
}

// Apex returns the zone's apex records (SOA, NS and possibly other). If the
// apex can not be found (thereby making it an illegal DNS zone) it returns nil.
// Updating the zone's SOA serial, provided the apex exists:
//
//	z.Apex.RR[TypeSOA][0].(*SOA).Serial++
//
// Note the a) this increment is not protected by locks and b) if you use DNSSEC
// you MUST resign the SOA record.
func (z *Zone) Apex() *ZoneData {
	apex, e := z.Find(z.Origin)
	if !e {
		fmt.Printf("%#v\n", apex)
		return nil
	}
	return apex
}

// Find looks up the ownername s in the zone and returns the
// data and true when an exact match is found. If an exact find isn't
// possible the first parent node with a non-nil Value is returned and
// the boolean is false.
func (z *Zone) Find(s string) (node *ZoneData, exact bool) {
	z.RLock()
	defer z.RUnlock()
	n, e := z.Radix.Find(toRadixName(s))
	if n == nil {
		return nil, false
	}
	node = n.Value.(*ZoneData)
	exact = e
	return
}

// FindFunc works like Find, but the function f is executed on
// each node which has a non-nil Value during the tree traversal.
// If f returns true, that node is returned.
func (z *Zone) FindFunc(s string, f func(interface{}) bool) (*ZoneData, bool, bool) {
	z.RLock()
	defer z.RUnlock()
	zd, e, b := z.Radix.FindFunc(toRadixName(s), f)
	if zd == nil {
		return nil, false, false
	}
	return zd.Value.(*ZoneData), e, b
}

func (z *Zone) isSubDomain(child string) bool {
	return compareLabelsSlice(z.olabels, strings.ToLower(child)) == len(z.olabels)
}

// Sign (re)signs the zone z with the given keys. 
// NSECs and RRSIGs are added as needed. 
// The public keys themselves are not added to the zone. 
// If config is nil DefaultSignatureConfig is used. The signatureConfig
// describes how the zone must be signed and if the SEP flag (for KSK)
// should be honored. If signatures approach their expriration time, they
// are refreshed with the current set of keys. Valid signatures are left alone.
//
// Basic use pattern for signing a zone with the default SignatureConfig:
//
//	// A single PublicKey/PrivateKey have been read from disk.
//	e := z.Sign(map[*dns.DNSKEY]dns.PrivateKey{pubkey.(*dns.DNSKEY): privkey}, nil)
//	if e != nil {
//		// signing error
//	}
//	// Admire your signed zone...
func (z *Zone) Sign(keys map[*DNSKEY]PrivateKey, config *SignatureConfig) error {
	z.Lock()
	z.ModTime = time.Now().UTC()
	defer z.Unlock()
	if config == nil {
		config = DefaultSignatureConfig
	}
	// Pre-calc the key tag
	keytags := make(map[*DNSKEY]uint16)
	for k, _ := range keys {
		keytags[k] = k.KeyTag()
	}

	errChan := make(chan error)
	radChan := make(chan *radix.Radix, config.SignerRoutines*2)

	// Start the signer goroutines
	wg := new(sync.WaitGroup)
	wg.Add(config.SignerRoutines)
	for i := 0; i < config.SignerRoutines; i++ {
		go signerRoutine(wg, keys, keytags, config, radChan, errChan)
	}

	apex, e := z.Radix.Find(toRadixName(z.Origin))
	if !e {
		return ErrSoa
	}
	config.Minttl = apex.Value.(*ZoneData).RR[TypeSOA][0].(*SOA).Minttl
	next := apex.Next()
	radChan <- apex

	var err error
Sign:
	for next.Value.(*ZoneData).Name != z.Origin {
		select {
		case err = <-errChan:
			break Sign
		default:
			radChan <- next
			next = next.Next()
		}
	}
	close(radChan)
	close(errChan)
	if err != nil {
		return err
	}
	wg.Wait()
	return nil
}

// signerRoutine is a small helper routine to make the concurrent signing work.
func signerRoutine(wg *sync.WaitGroup, keys map[*DNSKEY]PrivateKey, keytags map[*DNSKEY]uint16, config *SignatureConfig, in chan *radix.Radix, err chan error) {
	defer wg.Done()
	for {
		select {
		case data, ok := <-in:
			if !ok {
				return
			}
			e := data.Value.(*ZoneData).Sign(data.Next().Value.(*ZoneData).Name, keys, keytags, config)
			if e != nil {
				err <- e
				return
			}
		}
	}
}

// Sign signs a single ZoneData node. The zonedata itself is locked for writing,
// during the execution. It is important that the nodes' next record does not
// change. The caller must take care that the zone itself is also locked for writing.
// For a more complete description see zone.Sign. 
// Note, because this method has no (direct)
// access to the zone's SOA record, the SOA's Minttl value should be set in *config.
func (node *ZoneData) Sign(next string, keys map[*DNSKEY]PrivateKey, keytags map[*DNSKEY]uint16, config *SignatureConfig) error {
	node.Lock()
	defer node.Unlock()

	n, nsecok := node.RR[TypeNSEC]
	bitmap := []uint16{TypeNSEC, TypeRRSIG}
	bitmapEqual := true
	for t, _ := range node.RR {
		if nsecok {
			// Check if the current (if available) nsec has these types too
			// Grr O(n^2)
			found := false
			for _, v := range n[0].(*NSEC).TypeBitMap {
				if v == t {
					found = true
					break
				}
				if v > t { // It is sorted, so by now we haven't found it
					found = false
					break
				}
			}
			if !found {
				bitmapEqual = false
			}
		}
		if t == TypeNSEC || t == TypeRRSIG {
			continue
		}
		bitmap = append(bitmap, t)

	}
	sort.Sort(uint16Slice(bitmap))

	if nsecok {
		// There is an NSEC, check if it still points to the correct next node.
		// Secondly the type bitmap may have changed.
		// TODO(mg): actually checked the types in the map
		if n[0].(*NSEC).NextDomain != next || !bitmapEqual {
			n[0].(*NSEC).NextDomain = next
			n[0].(*NSEC).TypeBitMap = bitmap
			node.Signatures[TypeNSEC] = nil // drop all sigs
		}
	} else {
		// No NSEC at all, create one
		nsec := &NSEC{Hdr: RR_Header{node.Name, TypeNSEC, ClassINET, config.Minttl, 0}, NextDomain: next}
		nsec.TypeBitMap = bitmap
		node.RR[TypeNSEC] = []RR{nsec}
		node.Signatures[TypeNSEC] = nil // drop all sigs (just in case)
	}

	// Walk all keys, and check the sigs
	now := time.Now().UTC()
	for k, p := range keys {
		for t, rrset := range node.RR {
			if k.Flags&SEP == SEP {
				if _, ok := rrset[0].(*DNSKEY); !ok {
					// only sign keys with SEP keys
					continue
				}
			}
			if node.NonAuth == true {
				_, ok1 := rrset[0].(*DS)
				_, ok2 := rrset[0].(*NSEC)
				if !ok1 && !ok2 {
					continue
				}
			}

			j, q := signatures(node.Signatures[t], keytags[k])
			if q == nil || now.Sub(uint32ToTime(q.Expiration)) < config.Refresh { // no there, are almost expired
				s := new(RRSIG)
				s.SignerName = k.Hdr.Name
				s.Hdr.Ttl = k.Hdr.Ttl
				s.Hdr.Class = ClassINET
				s.Algorithm = k.Algorithm
				s.KeyTag = keytags[k]
				s.Inception = timeToUint32(now.Add(-config.InceptionOffset))
				s.Expiration = timeToUint32(now.Add(jitterDuration(config.Jitter)).Add(config.Validity))
				e := s.Sign(p, rrset)
				if e != nil {
					return e
				}
				if q != nil {
					node.Signatures[t][j] = s // replace the signature
				} else {
					node.Signatures[t] = append(node.Signatures[t], s) // add it
				}
			}
		}
	}
	// All signatures have been made are refreshed. Now check the all signatures for expiraton
	for i, s := range node.Signatures {
		// s is another slice
		for i1, s1 := range s {
			if now.Sub(uint32ToTime(s1.Expiration)) < config.Refresh {
				// can only happen if made with an unknown key, drop the sig
				node.Signatures[i] = append(node.Signatures[i][:i1], node.Signatures[i][i1+1:]...)
			}
		}
	}
	return nil
}

// Return the signature for the typecovered and make with the keytag. It
// returns the index of the RRSIG and the RRSIG itself.
func signatures(signatures []*RRSIG, keytag uint16) (int, *RRSIG) {
	for i, s := range signatures {
		if s.KeyTag == keytag {
			return i, s
		}
	}
	return 0, nil
}

// timeToUint32 translates a time.Time to a 32 bit value which                      
// can be used as the RRSIG's inception or expiration times.
func timeToUint32(t time.Time) uint32 {
	mod := (t.Unix() / year68) - 1
	if mod < 0 {
		mod = 0
	}
	return uint32(t.Unix() - (mod * year68))
}

// uint32ToTime translates a uint32 to a time.Time
func uint32ToTime(t uint32) time.Time {
	// uint32 to duration and then add it to epoch(0)
	mod := (time.Now().Unix() / year68) - 1
	if mod < 0 {
		mod = 0
	}
	duration := time.Duration((mod * year68) * int64(t))
	return time.Unix(0, 0).Add(duration)
}

// jitterTime returns a random +/- jitter
func jitterDuration(d time.Duration) time.Duration {
	jitter := rand.Intn(int(d))
	if rand.Intn(1) == 1 {
		return time.Duration(jitter)
	}
	return -time.Duration(jitter)
}

// compareLabels behaves exactly as CompareLabels expect that l1 is already
// a tokenize (in labels) version of the domain name. This saves memory and is faster.
func compareLabelsSlice(l1 []string, s2 string) (n int) {
	l2 := SplitLabels(s2)

	x1 := len(l1) - 1
	x2 := len(l2) - 1
	for {
		if x1 < 0 || x2 < 0 {
			break
		}
		if l1[x1] == l2[x2] {
			n++
		} else {
			break
		}
		x1--
		x2--
	}
	return
}
