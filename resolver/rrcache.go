package resolver

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"time"

	"github.com/safing/portbase/log"
	"github.com/safing/portmaster/nameserver/nsutil"
	"github.com/safing/portmaster/netenv"

	"github.com/miekg/dns"
)

// RRCache is a single-use structure to hold a DNS response.
// Persistence is handled through NameRecords because of a limitation of the
// underlying dns library.
//nolint:maligned // TODO
type RRCache struct {
	// Respnse Header
	Domain   string
	Question dns.Type
	RCode    int

	// Response Content
	Answer  []dns.RR
	Ns      []dns.RR
	Extra   []dns.RR
	Expires int64

	// Source Information
	Server      string
	ServerScope int8
	ServerInfo  string

	// Metadata about the request and handling
	ServedFromCache bool
	RequestingNew   bool
	IsBackup        bool
	Filtered        bool
	FilteredEntries []string

	// Modified holds when this entry was last changed, ie. saved to database.
	// This field is only populated when the entry comes from the cache.
	Modified int64
}

// ID returns the ID of the RRCache consisting of the domain and question type.
func (rrCache *RRCache) ID() string {
	return rrCache.Domain + rrCache.Question.String()
}

// Expired returns whether the record has expired.
func (rrCache *RRCache) Expired() bool {
	return rrCache.Expires <= time.Now().Unix()
}

// ExpiresSoon returns whether the record will expire soon and should already be refreshed.
func (rrCache *RRCache) ExpiresSoon() bool {
	return rrCache.Expires <= time.Now().Unix()+refreshTTL
}

// Clean sets all TTLs to 17 and sets cache expiry with specified minimum.
func (rrCache *RRCache) Clean(minExpires uint32) {
	var lowestTTL uint32 = 0xFFFFFFFF
	var header *dns.RR_Header

	// set TTLs to 17
	// TODO: double append? is there something more elegant?
	for _, rr := range append(rrCache.Answer, append(rrCache.Ns, rrCache.Extra...)...) {
		header = rr.Header()
		if lowestTTL > header.Ttl {
			lowestTTL = header.Ttl
		}
		header.Ttl = 17
	}

	// TTL range limits
	switch {
	case lowestTTL < minExpires:
		lowestTTL = minExpires
	case lowestTTL > maxTTL:
		lowestTTL = maxTTL
	}

	// Adjust return code if there are no answers
	if rrCache.RCode == dns.RcodeSuccess &&
		len(rrCache.Answer) == 0 {
		rrCache.RCode = dns.RcodeNameError
	}

	// shorten caching
	switch {
	case rrCache.RCode != dns.RcodeSuccess:
		// Any sort of error.
		lowestTTL = 10
	case netenv.IsConnectivityDomain(rrCache.Domain):
		// Responses from these domains might change very quickly depending on the environment.
		lowestTTL = 3
	case !netenv.Online():
		// Not being fully online could mean that we get funny responses.
		lowestTTL = 60
	}

	// log.Tracef("lowest TTL is %d", lowestTTL)
	rrCache.Expires = time.Now().Unix() + int64(lowestTTL)
}

// ExportAllARecords return of a list of all A and AAAA IP addresses.
func (rrCache *RRCache) ExportAllARecords() (ips []net.IP) {
	for _, rr := range rrCache.Answer {
		if rr.Header().Class != dns.ClassINET {
			continue
		}

		switch rr.Header().Rrtype {
		case dns.TypeA:
			aRecord, ok := rr.(*dns.A)
			if ok {
				ips = append(ips, aRecord.A)
			}
		case dns.TypeAAAA:
			aaaaRecord, ok := rr.(*dns.AAAA)
			if ok {
				ips = append(ips, aaaaRecord.AAAA)
			}
		}
	}
	return
}

// ToNameRecord converts the RRCache to a NameRecord for cleaner persistence.
func (rrCache *RRCache) ToNameRecord() *NameRecord {
	new := &NameRecord{
		Domain:      rrCache.Domain,
		Question:    rrCache.Question.String(),
		RCode:       rrCache.RCode,
		Expires:     rrCache.Expires,
		Server:      rrCache.Server,
		ServerScope: rrCache.ServerScope,
		ServerInfo:  rrCache.ServerInfo,
	}

	// stringify RR entries
	for _, entry := range rrCache.Answer {
		new.Answer = append(new.Answer, entry.String())
	}
	for _, entry := range rrCache.Ns {
		new.Ns = append(new.Ns, entry.String())
	}
	for _, entry := range rrCache.Extra {
		new.Extra = append(new.Extra, entry.String())
	}

	return new
}

// rcodeIsCacheable returns whether a record with the given RCode should be cached.
func rcodeIsCacheable(rCode int) bool {
	switch rCode {
	case dns.RcodeSuccess, dns.RcodeNameError, dns.RcodeRefused:
		return true
	default:
		return false
	}
}

// Cacheable returns whether the record should be cached.
func (rrCache *RRCache) Cacheable() bool {
	return rcodeIsCacheable(rrCache.RCode)
}

// Save saves the RRCache to the database as a NameRecord.
func (rrCache *RRCache) Save() error {
	if !rrCache.Cacheable() {
		return nil
	}

	return rrCache.ToNameRecord().Save()
}

// GetRRCache tries to load the corresponding NameRecord from the database and convert it.
func GetRRCache(domain string, question dns.Type) (*RRCache, error) {
	rrCache := &RRCache{
		Domain:   domain,
		Question: question,
	}

	nameRecord, err := GetNameRecord(domain, question.String())
	if err != nil {
		return nil, err
	}

	rrCache.RCode = nameRecord.RCode
	rrCache.Expires = nameRecord.Expires
	for _, entry := range nameRecord.Answer {
		rrCache.Answer = parseRR(rrCache.Answer, entry)
	}
	for _, entry := range nameRecord.Ns {
		rrCache.Ns = parseRR(rrCache.Ns, entry)
	}
	for _, entry := range nameRecord.Extra {
		rrCache.Extra = parseRR(rrCache.Extra, entry)
	}

	rrCache.Server = nameRecord.Server
	rrCache.ServerScope = nameRecord.ServerScope
	rrCache.ServerInfo = nameRecord.ServerInfo
	rrCache.ServedFromCache = true
	rrCache.Modified = nameRecord.Meta().Modified
	return rrCache, nil
}

func parseRR(section []dns.RR, entry string) []dns.RR {
	rr, err := dns.NewRR(entry)
	switch {
	case err != nil:
		log.Warningf("resolver: failed to parse cached record %q: %s", entry, err)
	case rr == nil:
		log.Warningf("resolver: failed to parse cached record %q: resulted in nil record", entry)
	default:
		return append(section, rr)
	}
	return section
}

// Flags formats ServedFromCache and RequestingNew to a condensed, flag-like format.
func (rrCache *RRCache) Flags() string {
	var s string
	if rrCache.ServedFromCache {
		s += "C"
	}
	if rrCache.RequestingNew {
		s += "R"
	}
	if rrCache.IsBackup {
		s += "B"
	}
	if rrCache.Filtered {
		s += "F"
	}

	if s != "" {
		return fmt.Sprintf(" [%s]", s)
	}
	return ""
}

// ShallowCopy returns a shallow copy of the cache. slices are not copied, but referenced.
func (rrCache *RRCache) ShallowCopy() *RRCache {
	return &RRCache{
		Domain:   rrCache.Domain,
		Question: rrCache.Question,
		RCode:    rrCache.RCode,

		Answer:  rrCache.Answer,
		Ns:      rrCache.Ns,
		Extra:   rrCache.Extra,
		Expires: rrCache.Expires,

		Server:      rrCache.Server,
		ServerScope: rrCache.ServerScope,
		ServerInfo:  rrCache.ServerInfo,

		ServedFromCache: rrCache.ServedFromCache,
		RequestingNew:   rrCache.RequestingNew,
		IsBackup:        rrCache.IsBackup,
		Filtered:        rrCache.Filtered,
		FilteredEntries: rrCache.FilteredEntries,
		Modified:        rrCache.Modified,
	}
}

// ReplaceAnswerNames is a helper function that replaces all answer names, that
// match the query domain, with another value. This is used to support handling
// non-standard query names, which are resolved normalized, but have to be
// reverted back for the origin non-standard query name in order for the
// clients to recognize the response.
func (rrCache *RRCache) ReplaceAnswerNames(fqdn string) {
	for _, answer := range rrCache.Answer {
		if answer.Header().Name == rrCache.Domain {
			answer.Header().Name = fqdn
		}
	}
}

// ReplyWithDNS creates a new reply to the given query with the data from the RRCache, and additional informational records.
func (rrCache *RRCache) ReplyWithDNS(ctx context.Context, request *dns.Msg) *dns.Msg {
	// reply to query
	reply := new(dns.Msg)
	reply.SetRcode(request, rrCache.RCode)
	reply.Answer = rrCache.Answer
	reply.Ns = rrCache.Ns
	reply.Extra = rrCache.Extra

	// Randomize the order of the answer records a little to allow dumb clients
	// (who only look at the first record) to reliably connect.
	for i := range reply.Answer {
		j := rand.Intn(i + 1)
		reply.Answer[i], reply.Answer[j] = reply.Answer[j], reply.Answer[i]
	}

	return reply
}

// GetExtraRRs returns a slice of RRs with additional informational records.
func (rrCache *RRCache) GetExtraRRs(ctx context.Context, query *dns.Msg) (extra []dns.RR) {
	// Add cache status and source of data.
	if rrCache.ServedFromCache {
		extra = addExtra(ctx, extra, "served from cache, resolved by "+rrCache.ServerInfo)
	} else {
		extra = addExtra(ctx, extra, "freshly resolved by "+rrCache.ServerInfo)
	}

	// Add expiry and cache information.
	if rrCache.Expired() {
		extra = addExtra(ctx, extra, fmt.Sprintf("record expired since %s", time.Since(time.Unix(rrCache.Expires, 0)).Round(time.Second)))
	} else {
		extra = addExtra(ctx, extra, fmt.Sprintf("record valid for %s", time.Until(time.Unix(rrCache.Expires, 0)).Round(time.Second)))
	}
	if rrCache.RequestingNew {
		extra = addExtra(ctx, extra, "async request to refresh the cache has been started")
	}
	if rrCache.IsBackup {
		extra = addExtra(ctx, extra, "this record is served because a fresh request was unsuccessful")
	}

	// Add information about filtered entries.
	if rrCache.Filtered {
		if len(rrCache.FilteredEntries) > 1 {
			extra = addExtra(ctx, extra, fmt.Sprintf("%d records have been filtered", len(rrCache.FilteredEntries)))
		} else {
			extra = addExtra(ctx, extra, fmt.Sprintf("%d record has been filtered", len(rrCache.FilteredEntries)))
		}
	}

	return extra
}

func addExtra(ctx context.Context, extra []dns.RR, msg string) []dns.RR {
	rr, err := nsutil.MakeMessageRecord(log.InfoLevel, msg)
	if err != nil {
		log.Tracer(ctx).Warningf("resolver: failed to add informational record to reply: %s", err)
		return extra
	}
	extra = append(extra, rr)
	return extra
}
