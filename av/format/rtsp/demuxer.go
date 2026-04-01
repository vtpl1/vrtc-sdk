// Package rtsp implements a RTSP demuxer over TCP interleaved RTP.
//
// Supported codecs:
//   - H264 (RFC 6184)
//   - H265 (RFC 7798)
//
// The implementation is intentionally self-contained (no gortsplib dependency)
// and is based on the same protocol flow: OPTIONS -> DESCRIBE -> SETUP -> PLAY.
package rtsp

import (
	"bufio"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/codec"
	"github.com/vtpl1/vrtc-sdk/av/codec/h264parser"
	"github.com/vtpl1/vrtc-sdk/av/codec/h265parser"
)

const (
	defaultRTSPPort = "554"
	defaultTimeout  = 10 * time.Second
)

// Compile-time interface check.
var _ av.DemuxCloser = (*Demuxer)(nil)

// Demuxer reads packets from an RTSP source URL.
type Demuxer struct {
	sourceURL string
	timeNow   func() time.Time

	mu      sync.Mutex
	started bool
	closed  bool

	conn      net.Conn
	reader    *bufio.Reader
	requestID int
	sessionID string
	baseURL   *url.URL
	auth      authState

	tracks       []*rtspTrack
	trackByRTPCh map[uint8]*rtspTrack
	streams      []av.Stream
	pending      []av.Packet
}

// NewDemuxer creates a RTSP demuxer for sourceID (RTSP URL).
func NewDemuxer(sourceID string) *Demuxer {
	return &Demuxer{
		sourceURL: sourceID,
		timeNow:   time.Now,
	}
}

// NewDemuxerFactory returns an av.DemuxerFactory that creates RTSP demuxers.
func NewDemuxerFactory() av.DemuxerFactory {
	return func(_ context.Context, sourceID string) (av.DemuxCloser, error) {
		return NewDemuxer(sourceID), nil
	}
}

// GetCodecs connects and negotiates the RTSP session.
func (d *Demuxer) GetCodecs(ctx context.Context) ([]av.Stream, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.started {
		return append([]av.Stream(nil), d.streams...), nil
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	sourceURL, err := url.Parse(d.sourceURL)
	if err != nil {
		return nil, fmt.Errorf("rtsp: parse source url: %w", err)
	}

	if sourceURL.Scheme != "rtsp" && sourceURL.Scheme != "rtsps" {
		return nil, fmt.Errorf("rtsp: unsupported URL scheme: %s", sourceURL.Scheme)
	}

	conn, err := d.dial(ctx, sourceURL)
	if err != nil {
		return nil, err
	}

	d.conn = conn
	d.reader = bufio.NewReader(conn)
	d.requestID = 1
	d.baseURL = sourceURL
	d.auth = authState{username: sourceURL.User.Username()}

	if pw, ok := sourceURL.User.Password(); ok {
		d.auth.password = pw
	}

	if err := d.options(ctx, sourceURL); err != nil {
		_ = d.closeLocked()

		return nil, err
	}

	sdpBody, err := d.describe(ctx, sourceURL)
	if err != nil {
		_ = d.closeLocked()

		return nil, err
	}

	tracks, streams, err := d.buildTracks(sdpBody)
	if err != nil {
		_ = d.closeLocked()

		return nil, err
	}

	if len(tracks) == 0 {
		_ = d.closeLocked()

		return nil, ErrNoSupportedVideoTrack
	}

	for idx, tr := range tracks {
		tr.rtpChannel = uint8(idx * 2)

		if err := d.setupTrack(ctx, sourceURL, tr); err != nil {
			_ = d.closeLocked()

			return nil, err
		}
	}

	if err := d.play(ctx, sourceURL); err != nil {
		_ = d.closeLocked()

		return nil, err
	}

	d.trackByRTPCh = make(map[uint8]*rtspTrack, len(tracks))
	for _, tr := range tracks {
		d.trackByRTPCh[tr.rtpChannel] = tr
	}

	d.tracks = tracks
	d.streams = streams
	d.started = true

	return append([]av.Stream(nil), d.streams...), nil
}

// ReadPacket reads one packet from the RTSP session.
func (d *Demuxer) ReadPacket(ctx context.Context) (av.Packet, error) {
	d.mu.Lock()
	if !d.started {
		d.mu.Unlock()

		return av.Packet{}, ErrNotStarted
	}

	if len(d.pending) > 0 {
		pkt := d.pending[0]
		d.pending = d.pending[1:]
		d.mu.Unlock()

		return pkt, nil
	}

	conn := d.conn
	reader := d.reader
	trackByRTPCh := d.trackByRTPCh
	timeNow := d.timeNow
	d.mu.Unlock()

	for {
		if err := ctx.Err(); err != nil {
			return av.Packet{}, err
		}

		_ = conn.SetReadDeadline(timeNow().Add(1 * time.Second))

		ch, payload, err := readInterleavedFrame(reader)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}

			return av.Packet{}, err
		}

		// odd channels are RTCP.
		if (ch % 2) == 1 {
			continue
		}

		tr, ok := trackByRTPCh[ch]
		if !ok {
			continue
		}

		var rtpPacket rtp.Packet
		if err := rtpPacket.Unmarshal(payload); err != nil {
			continue
		}

		pkts, err := tr.decodeRTP(&rtpPacket)
		if err != nil {
			if err == errNeedMorePackets {
				continue
			}

			// Treat packet-level decode errors as non-fatal stream glitches.
			continue
		}

		if len(pkts) == 0 {
			continue
		}

		d.mu.Lock()
		if len(pkts) > 1 {
			d.pending = append(d.pending, pkts[1:]...)
		}
		d.mu.Unlock()

		return pkts[0], nil
	}
}

// Close tears down the RTSP session.
func (d *Demuxer) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.closeLocked()
}

func (d *Demuxer) closeLocked() error {
	if d.closed {
		return nil
	}
	d.closed = true

	var retErr error

	if d.conn != nil {
		if d.started && d.baseURL != nil {
			// best effort teardown
			_, _ = d.doRequest(context.Background(), "TEARDOWN", d.baseURL.String(), nil, nil)
		}

		if err := d.conn.Close(); err != nil {
			retErr = err
		}
	}

	return retErr
}

func (d *Demuxer) dial(ctx context.Context, u *url.URL) (net.Conn, error) {
	host := u.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, defaultRTSPPort)
	}

	dialer := net.Dialer{Timeout: defaultTimeout}

	conn, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("rtsp: dial %s: %w", host, err)
	}

	return conn, nil
}

func (d *Demuxer) options(ctx context.Context, u *url.URL) error {
	resp, err := d.doRequest(ctx, "OPTIONS", u.String(), nil, nil)
	if err != nil {
		return err
	}

	if resp.statusCode != 200 {
		return fmt.Errorf("rtsp: OPTIONS failed: %d %s", resp.statusCode, resp.status)
	}

	return nil
}

func (d *Demuxer) describe(ctx context.Context, u *url.URL) (string, error) {
	headers := map[string]string{
		"Accept": "application/sdp",
	}

	resp, err := d.doRequest(ctx, "DESCRIBE", u.String(), headers, nil)
	if err != nil {
		return "", err
	}

	if resp.statusCode != 200 {
		return "", fmt.Errorf("rtsp: DESCRIBE failed: %d %s", resp.statusCode, resp.status)
	}

	contentBase := resp.headers.Get("Content-Base")
	if contentBase != "" {
		if cbURL, err := url.Parse(contentBase); err == nil {
			d.baseURL = cbURL
		}
	}

	return string(resp.body), nil
}

func (d *Demuxer) setupTrack(ctx context.Context, reqURL *url.URL, tr *rtspTrack) error {
	setupURL := resolveControlURL(reqURL, tr.controlURL)
	headers := map[string]string{
		"Transport": fmt.Sprintf("RTP/AVP/TCP;unicast;interleaved=%d-%d", tr.rtpChannel, tr.rtpChannel+1),
	}

	if d.sessionID != "" {
		headers["Session"] = d.sessionID
	}

	resp, err := d.doRequest(ctx, "SETUP", setupURL, headers, nil)
	if err != nil {
		return err
	}

	if resp.statusCode != 200 {
		return fmt.Errorf("rtsp: SETUP failed: %d %s", resp.statusCode, resp.status)
	}

	if d.sessionID == "" {
		session := resp.headers.Get("Session")
		if i := strings.IndexByte(session, ';'); i >= 0 {
			session = session[:i]
		}

		d.sessionID = strings.TrimSpace(session)
	}

	return nil
}

func (d *Demuxer) play(ctx context.Context, u *url.URL) error {
	headers := map[string]string{}
	if d.sessionID != "" {
		headers["Session"] = d.sessionID
	}

	resp, err := d.doRequest(ctx, "PLAY", u.String(), headers, nil)
	if err != nil {
		return err
	}

	if resp.statusCode != 200 {
		return fmt.Errorf("rtsp: PLAY failed: %d %s", resp.statusCode, resp.status)
	}

	return nil
}

func (d *Demuxer) doRequest(
	ctx context.Context,
	method string,
	requestURI string,
	extraHeaders map[string]string,
	body []byte,
) (*rtspResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	for attempt := 0; attempt < 2; attempt++ {
		cseq := d.requestID
		d.requestID++

		headers := make(map[string]string, len(extraHeaders)+5)
		headers["CSeq"] = strconv.Itoa(cseq)
		headers["User-Agent"] = "vrtc-sdk-rtsp"

		if d.sessionID != "" {
			headers["Session"] = d.sessionID
		}

		for k, v := range extraHeaders {
			headers[k] = v
		}

		if auth := d.auth.authorization(method, requestURI); auth != "" {
			headers["Authorization"] = auth
		}

		if len(body) > 0 {
			headers["Content-Length"] = strconv.Itoa(len(body))
		}

		if err := writeRTSPRequest(d.conn, method, requestURI, headers, body); err != nil {
			return nil, fmt.Errorf("rtsp: write %s: %w", method, err)
		}

		resp, err := readRTSPResponse(d.reader)
		if err != nil {
			return nil, fmt.Errorf("rtsp: read %s response: %w", method, err)
		}

		if resp.statusCode == 401 && d.auth.canAttemptAuth() && d.auth.applyChallenge(resp.headers.Get("WWW-Authenticate")) {
			continue
		}

		return resp, nil
	}

	return nil, fmt.Errorf("rtsp: authentication failed")
}

func (d *Demuxer) buildTracks(sdpBody string) ([]*rtspTrack, []av.Stream, error) {
	cds, err := codec.SdpToCodecs(sdpBody)
	if err != nil {
		return nil, nil, fmt.Errorf("rtsp: parse SDP: %w", err)
	}

	tracks := make([]*rtspTrack, 0, len(cds))
	streams := make([]av.Stream, 0, len(cds))

	for _, cd := range cds {
		track, err := newTrack(uint16(len(streams)), cd)
		if err != nil {
			continue
		}

		tracks = append(tracks, track)
		streams = append(streams, av.Stream{Idx: track.idx, Codec: track.codec})
	}

	return tracks, streams, nil
}

// rtspTrack maps one RTSP media track to av stream output.
type rtspTrack struct {
	idx        uint16
	codec      av.CodecData
	codecType  av.CodecType
	controlURL string

	rtpChannel uint8
	clockRate  int

	h264Decoder *h264RTPDecoder
	h265Parser  h265parser.Parser

	haveRTPBase bool
	lastRTP     uint32
	totalRTP    int64
	lastDTS     time.Duration
	haveLastDTS bool
	lastDur     time.Duration
}

func newTrack(idx uint16, cd av.CodecData) (*rtspTrack, error) {
	tr := &rtspTrack{
		idx:       idx,
		codec:     cd,
		codecType: cd.Type(),
		clockRate: 90000,
	}

	switch v := cd.(type) {
	case h264parser.CodecData:
		tr.controlURL = v.ControlURL
		tr.h264Decoder = &h264RTPDecoder{}

	case h265parser.CodecData:
		tr.controlURL = v.ControlURL

	default:
		return nil, fmt.Errorf("unsupported codec type: %s", cd.Type())
	}

	return tr, nil
}

func (t *rtspTrack) decodeRTP(pkt *rtp.Packet) ([]av.Packet, error) {
	dts := t.decodeDTS(pkt.Timestamp)

	switch t.codecType {
	case av.H264:
		return t.decodeH264(pkt, dts)
	case av.H265:
		return t.decodeH265(pkt, dts)
	default:
		return nil, nil
	}
}

func (t *rtspTrack) decodeDTS(ts uint32) time.Duration {
	if !t.haveRTPBase {
		t.haveRTPBase = true
		t.lastRTP = ts
		t.totalRTP = 0

		return 0
	}

	t.totalRTP += int64(int32(ts - t.lastRTP))
	t.lastRTP = ts

	dts := time.Duration(t.totalRTP) * time.Second / time.Duration(t.clockRate)
	if t.haveLastDTS && dts < t.lastDTS {
		return t.lastDTS
	}

	return dts
}

func (t *rtspTrack) decodeH264(pkt *rtp.Packet, dts time.Duration) ([]av.Packet, error) {
	nalus, err := t.h264Decoder.Decode(pkt)
	if err != nil {
		return nil, err
	}

	if len(nalus) == 0 {
		return nil, nil
	}

	payload := h264parser.AnnexBToAVCC(nalus)
	dur := t.estimateDuration(payload, dts)
	key := h264AccessUnitIsKeyFrame(nalus)

	out := av.Packet{
		Idx:       t.idx,
		CodecType: av.H264,
		DTS:       dts,
		Duration:  dur,
		KeyFrame:  key,
		Data:      payload,
	}

	t.lastDTS = dts
	t.haveLastDTS = true
	t.lastDur = dur

	return []av.Packet{out}, nil
}

func (t *rtspTrack) decodeH265(pkt *rtp.Packet, dts time.Duration) ([]av.Packet, error) {
	au, ready, err := t.h265Parser.PushRTP(pkt.Payload)
	if err != nil {
		return nil, err
	}

	if !ready || au == nil || len(au.NALUs) == 0 {
		return nil, errNeedMorePackets
	}

	payload := h265parser.AnnexBToAVCC(au.NALUs)
	dur := t.estimateDuration(payload, dts)

	out := av.Packet{
		Idx:       t.idx,
		CodecType: av.H265,
		DTS:       dts,
		Duration:  dur,
		KeyFrame:  au.KeyFrame,
		Data:      payload,
	}

	t.lastDTS = dts
	t.haveLastDTS = true
	t.lastDur = dur

	return []av.Packet{out}, nil
}

func (t *rtspTrack) estimateDuration(data []byte, dts time.Duration) time.Duration {
	if t.haveLastDTS && dts > t.lastDTS {
		return dts - t.lastDTS
	}

	if t.lastDur > 0 {
		return t.lastDur
	}

	if pd, ok := t.codec.(interface{ PacketDuration([]byte) time.Duration }); ok {
		d := pd.PacketDuration(data)
		if d > 0 {
			return d
		}
	}

	return 40 * time.Millisecond
}

func h264AccessUnitIsKeyFrame(nalus [][]byte) bool {
	for _, nalu := range nalus {
		if len(nalu) == 0 {
			continue
		}

		naluType := av.H264NaluType(nalu[0]) & av.H264NALTypeMask
		if naluType == av.H264_NAL_IDR_SLICE {
			return true
		}
	}

	return false
}

func resolveControlURL(base *url.URL, control string) string {
	if control == "" || control == "*" {
		return base.String()
	}

	if strings.HasPrefix(control, "/") {
		baseCopy := *base
		baseCopy.Path = control
		baseCopy.RawPath = ""
		baseCopy.RawQuery = ""
		baseCopy.Fragment = ""

		return baseCopy.String()
	}

	if strings.HasPrefix(control, "rtsp://") || strings.HasPrefix(control, "rtsps://") {
		if u, err := url.Parse(control); err == nil {
			u.Host = base.Host
			u.User = base.User

			return u.String()
		}
	}

	baseCopy := *base
	baseStr := baseCopy.String()

	if control[0] != '?' && control[0] != '/' && !strings.HasSuffix(baseStr, "/") {
		baseStr += "/"
	}

	if u, err := url.Parse(baseStr + control); err == nil {
		return u.String()
	}

	return base.String()
}

type rtspResponse struct {
	statusCode int
	status     string
	headers    textproto.MIMEHeader
	body       []byte
}

func writeRTSPRequest(
	w io.Writer,
	method string,
	uri string,
	headers map[string]string,
	body []byte,
) error {
	if _, err := fmt.Fprintf(w, "%s %s RTSP/1.0\r\n", method, uri); err != nil {
		return err
	}

	for k, v := range headers {
		if _, err := fmt.Fprintf(w, "%s: %s\r\n", k, v); err != nil {
			return err
		}
	}

	if _, err := io.WriteString(w, "\r\n"); err != nil {
		return err
	}

	if len(body) > 0 {
		if _, err := w.Write(body); err != nil {
			return err
		}
	}

	return nil
}

func readRTSPResponse(r *bufio.Reader) (*rtspResponse, error) {
	tp := textproto.NewReader(r)

	line, err := tp.ReadLine()
	if err != nil {
		return nil, err
	}

	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "RTSP/") {
		return nil, ErrUnexpectedStatusLine
	}

	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, ErrUnexpectedStatusLine
	}

	headers, err := tp.ReadMIMEHeader()
	if err != nil {
		return nil, err
	}

	resp := &rtspResponse{
		statusCode: code,
		status:     strings.TrimSpace(strings.TrimPrefix(line, parts[0]+" "+parts[1])),
		headers:    headers,
	}

	if clStr := headers.Get("Content-Length"); clStr != "" {
		cl, err := strconv.Atoi(strings.TrimSpace(clStr))
		if err == nil && cl > 0 {
			resp.body = make([]byte, cl)
			if _, err := io.ReadFull(r, resp.body); err != nil {
				return nil, err
			}
		}
	}

	return resp, nil
}

func readInterleavedFrame(r *bufio.Reader) (uint8, []byte, error) {
	b, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}

	if b != '$' {
		return 0, nil, ErrUnexpectedInterleaved
	}

	ch, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}

	var lenBuf [2]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return 0, nil, err
	}

	n := int(binary.BigEndian.Uint16(lenBuf[:]))
	if n <= 0 {
		return 0, nil, fmt.Errorf("rtsp: empty interleaved frame")
	}

	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}

	return ch, payload, nil
}

type authKind int

const (
	authNone authKind = iota
	authBasic
	authDigest
)

type authState struct {
	kind authKind

	username string
	password string

	realm string
	nonce string
	qop   string
	nc    uint32
}

func (a *authState) canAttemptAuth() bool {
	return a.username != "" || a.password != ""
}

func (a *authState) applyChallenge(header string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}

	lower := strings.ToLower(header)
	if strings.HasPrefix(lower, "basic") {
		a.kind = authBasic

		return true
	}

	if !strings.HasPrefix(lower, "digest") {
		return false
	}

	params := parseAuthParams(header[len("Digest"):])
	a.kind = authDigest
	a.realm = params["realm"]
	a.nonce = params["nonce"]
	a.qop = params["qop"]
	a.nc = 0

	return a.nonce != ""
}

func (a *authState) authorization(method string, uri string) string {
	switch a.kind {
	case authBasic:
		plain := a.username + ":" + a.password

		return "Basic " + base64.StdEncoding.EncodeToString([]byte(plain))

	case authDigest:
		a.nc++

		ncHex := fmt.Sprintf("%08x", a.nc)
		cnonce := fmt.Sprintf("%08x", rand.Uint32())

		ha1 := md5Hex(a.username + ":" + a.realm + ":" + a.password)
		ha2 := md5Hex(method + ":" + uri)

		response := ""
		if strings.Contains(strings.ToLower(a.qop), "auth") {
			response = md5Hex(ha1 + ":" + a.nonce + ":" + ncHex + ":" + cnonce + ":auth:" + ha2)

			return fmt.Sprintf(
				`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s", qop=auth, nc=%s, cnonce="%s"`,
				a.username,
				a.realm,
				a.nonce,
				uri,
				response,
				ncHex,
				cnonce,
			)
		}

		response = md5Hex(ha1 + ":" + a.nonce + ":" + ha2)

		return fmt.Sprintf(
			`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s"`,
			a.username,
			a.realm,
			a.nonce,
			uri,
			response,
		)
	}

	return ""
}

func parseAuthParams(s string) map[string]string {
	out := make(map[string]string)

	parts := strings.Split(s, ",")
	for _, raw := range parts {
		kv := strings.SplitN(strings.TrimSpace(raw), "=", 2)
		if len(kv) != 2 {
			continue
		}

		key := strings.ToLower(strings.TrimSpace(kv[0]))
		val := strings.Trim(strings.TrimSpace(kv[1]), `"`)
		out[key] = val
	}

	return out
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))

	return fmt.Sprintf("%x", sum)
}
