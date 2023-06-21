package proxy

import (
	"fmt"
	"github.com/rekby/lets-proxy2/internal/contextlabel"
	"net"
	"net/http"
	"net/url"
	"strconv"

	"github.com/rekby/lets-proxy2/internal/log"

	zc "github.com/rekby/zapcontext"

	"go.uber.org/zap"

	"github.com/egorgasay/cidranger"
)

const (
	ConnectionID = "{{CONNECTION_ID}}"
	HTTPProto    = "{{HTTP_PROTO}}"
	SourceIP     = "{{SOURCE_IP}}"
	SourcePort   = "{{SOURCE_PORT}}"
	SourceIPPort = "{{SOURCE_IP}}:{{SOURCE_PORT}}"
)

const (
	ProtocolHTTP  = "http"
	ProtocolHTTPS = "https"
)

type DirectorChain []Director

func (c DirectorChain) Director(request *http.Request) error {
	for _, d := range c {
		err := d.Director(request)
		if err != nil {
			return err
		}
	}
	return nil
}

// skip nil directors
func NewDirectorChain(directors ...Director) DirectorChain {
	cnt := 0

	for _, item := range directors {
		if item != nil {
			cnt++
		}
	}

	ownDirectors := make(DirectorChain, 0, cnt)

	for _, item := range directors {
		if item != nil {
			ownDirectors = append(ownDirectors, item)
		}
	}

	return ownDirectors
}

type DirectorSameIP struct {
	Port string
}

func NewDirectorSameIP(port int) DirectorSameIP {
	return DirectorSameIP{strconv.Itoa(port)}
}

func (s DirectorSameIP) Director(request *http.Request) error {
	localAddr := request.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr)
	if request.URL == nil {
		request.URL = &url.URL{}
	}
	request.URL.Host = localAddr.IP.String() + ":" + s.Port
	zc.L(request.Context()).Debug("Set target as same ip",
		zap.Stringer("local_addr", localAddr), zap.String("dest_host", request.Host))
	return nil
}

type DirectorDestMap map[string]string

func (d DirectorDestMap) Director(request *http.Request) error {
	ctx := request.Context()

	type Stringer interface {
		String() string
	}

	localAddr := ctx.Value(http.LocalAddrContextKey).(Stringer).String()
	var dest string
	var ok bool
	if dest, ok = d[localAddr]; !ok {
		zc.L(ctx).Debug("Map director no matches, skip.")
		return nil
	}

	if request.URL == nil {
		request.URL = &url.URL{}
	}
	request.URL.Host = dest
	zc.L(ctx).Debug("Map director set dest", zap.String("host", request.URL.Host))
	return nil
}

func NewDirectorDestMap(m map[string]string) DirectorDestMap {
	res := make(DirectorDestMap, len(m))
	for k, v := range m {
		res[k] = v
	}
	return res
}

type DirectorHost string

func (d DirectorHost) Director(request *http.Request) error {
	if request.URL == nil {
		request.URL = &url.URL{}
	}
	request.URL.Host = string(d)
	return nil
}

func NewDirectorHost(host string) DirectorHost {
	return DirectorHost(host)
}

type DirectorSetHeaders map[string]string

func NewDirectorSetHeaders(m map[string]string) DirectorSetHeaders {
	res := make(DirectorSetHeaders, len(m))
	for k, v := range m {
		res[k] = v
	}
	return res
}

func (h DirectorSetHeaders) Director(request *http.Request) error {
	ctx := request.Context()
	host, port, errHostPort := net.SplitHostPort(request.RemoteAddr)
	log.DebugDPanicCtx(ctx, errHostPort, "Parse remote addr for headers", zap.String("host", host), zap.String("port", port))

	for name, headerVal := range h {
		var value string

		switch headerVal {
		case ConnectionID:
			value = request.Context().Value(contextlabel.ConnectionID).(string)
		case HTTPProto:
			if tls, ok := ctx.Value(contextlabel.TLSConnection).(bool); ok {
				if tls {
					value = ProtocolHTTPS
				} else {
					value = ProtocolHTTP
				}
			} else {
				value = "error protocol detection"
			}
		case SourceIP:
			value = host
		case SourceIPPort:
			value = host + ":" + port
		case SourcePort:
			value = port
		default:
			value = headerVal
		}

		if request.Header == nil {
			request.Header = make(http.Header)
		}

		request.Header.Set(name, value)
	}
	return nil
}

type HTTPHeader struct {
	Name  string
	Value string
}
type HTTPHeaders []HTTPHeader
type NetHeaders struct {
	IPNet   net.IPNet
	Headers HTTPHeaders
}
type Net struct {
	net.IPNet
}

func (n Net) Network() net.IPNet { return n.IPNet }

type DirectorSetHeadersByIP struct {
	allHeaders HeadersList
	cidranger.Ranger[HTTPHeaders]
}

type HeadersList interface {
	ShouldRemoveHeaderByIP(headerName string, ip net.IP) bool
	Add(headerName string, cidr net.IPNet)
	Len() int
	Iter(fn func(headerName string, ip net.IP))
}

type NetHeaderName struct {
	IPNet      net.IPNet
	HeaderName string
}

type HeadersStorageMap map[string][]net.IPNet

type HeadersStorageSlice []NetHeaderName

func newHeadersStorageSlice() *HeadersStorageSlice {
	s := make(HeadersStorageSlice, 0, 100)
	return &s
}

func newHeadersStorageMap() *HeadersStorageMap {
	m := make(HeadersStorageMap, 400)
	return &m
}

func (h *HeadersStorageSlice) ShouldRemoveHeaderByIP(headerName string, ip net.IP) bool {
	for _, netHeaders := range *h {
		if headerName == netHeaders.HeaderName && !netHeaders.IPNet.Contains(ip) {
			return true
		}
	}
	return false
}

func (h *HeadersStorageSlice) Add(headerName string, cidr net.IPNet) {
	*h = append(*h, NetHeaderName{
		IPNet:      cidr,
		HeaderName: headerName,
	})
}

func (h *HeadersStorageSlice) Len() int {
	return len(*h)
}

func (h *HeadersStorageMap) Iter(fn func(headerName string, ip net.IP)) {
	for headerName, netHeaders := range *h {
		for _, netHeader := range netHeaders {
			fn(headerName, netHeader.IP)
		}
	}
}

func (h *HeadersStorageMap) Add(headerName string, cidr net.IPNet) {
	(*h)[headerName] = append((*h)[headerName], cidr)
}

func (h *HeadersStorageMap) ShouldRemoveHeaderByIP(headerName string, ip net.IP) bool {
	nets, ok := (*h)[headerName]
	if !ok {
		return false
	}

	for _, network := range nets {
		if network.Contains(ip) {
			return true
		}
	}

	return false
}

func (h *HeadersStorageMap) Len() int {
	return len(*h)
}

func (h *HeadersStorageSlice) Iter(fn func(headerName string, ip net.IP)) {
	for _, netHeaders := range *h {
		fn(netHeaders.HeaderName, netHeaders.IPNet.IP)
	}
}

func NewDirectorSetHeadersByIP(m map[string]HTTPHeaders) (DirectorSetHeadersByIP, error) {
	var allHeaders HeadersList
	if len(m) > 200 {
		allHeaders = newHeadersStorageMap()
	} else {
		allHeaders = newHeadersStorageSlice()
	}

	ranger := cidranger.NewPCTrieRanger[HTTPHeaders]()
	for k, v := range m {
		_, subnet, err := net.ParseCIDR(k)
		if err != nil {
			return DirectorSetHeadersByIP{}, fmt.Errorf("can't parse CIDR: %v %w", k, err)
		}

		err = ranger.Insert(&Net{IPNet: *subnet}, v)
		if err != nil {
			return DirectorSetHeadersByIP{}, fmt.Errorf("can't insert into cidranger %w", err)
		}

		for _, header := range v {
			allHeaders.Add(header.Name, *subnet)
		}
	}
	return DirectorSetHeadersByIP{Ranger: ranger, allHeaders: allHeaders}, nil
}

func (h DirectorSetHeadersByIP) Director(request *http.Request) error {
	if request == nil {
		return fmt.Errorf("request is nil")
	}

	ctx := request.Context()
	host, port, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		zc.L(ctx).Debug("Split host port error", zap.Error(err), zap.String("host", host),
			zap.String("port", port))
	}

	ip := net.ParseIP(host)

	if len(request.Header) < h.allHeaders.Len() {
		for headerName := range request.Header {
			if h.allHeaders.ShouldRemoveHeaderByIP(headerName, ip) {
				delete(request.Header, headerName)
			}
		}
	} else {
		h.allHeaders.Iter(func(headerName string, ip net.IP) {
			_, ok := request.Header[headerName]
			if ok && h.allHeaders.ShouldRemoveHeaderByIP(headerName, ip) {
				delete(request.Header, headerName)
			}
		})
	}

	err = h.IterByIncomingNetworks(ip, func(network net.IPNet, value HTTPHeaders) error {
		if request.Header == nil {
			request.Header = make(http.Header)
		}

		for _, header := range value {
			request.Header[header.Name] = []string{header.Value}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("can't iterate cidranger %w", err)
	}

	return nil
}

type DirectorSetScheme string

func (d DirectorSetScheme) Director(req *http.Request) error {
	if req.URL == nil {
		req.URL = &url.URL{}
	}
	req.URL.Scheme = string(d)
	return nil
}

func NewSetSchemeDirector(scheme string) DirectorSetScheme {
	return DirectorSetScheme(scheme)
}
