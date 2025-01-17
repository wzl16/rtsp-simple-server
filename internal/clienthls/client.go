package clienthls

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aler9/gortsplib"
	"github.com/aler9/gortsplib/pkg/headers"
	"github.com/aler9/gortsplib/pkg/ringbuffer"
	"github.com/aler9/gortsplib/pkg/rtpaac"
	"github.com/aler9/gortsplib/pkg/rtph264"

	"github.com/aler9/rtsp-simple-server/internal/client"
	"github.com/aler9/rtsp-simple-server/internal/h264"
	"github.com/aler9/rtsp-simple-server/internal/logger"
	"github.com/aler9/rtsp-simple-server/internal/serverhls"
	"github.com/aler9/rtsp-simple-server/internal/stats"
)

const (
	// an offset is needed to
	// - avoid negative PTS values
	// - avoid PTS < DTS during startup
	ptsOffset = 2 * time.Second

	segmentMinAUCount    = 100
	closeCheckPeriod     = 1 * time.Second
	closeAfterInactivity = 60 * time.Second
)

const index = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<style>
#video {
	width: 600px;
	height: 600px;
	background: black;
}
</style>
</head>
<body>

<script src="https://cdn.jsdelivr.net/npm/hls.js@1.0.0"></script>
<video id="video" muted controls></video>
<script>

const create = () => {
	const video = document.getElementById('video');

	const hls = new Hls({
		progressive: false,
	});

	hls.on(Hls.Events.ERROR, (evt, data) => {
		if (data.fatal) {
			hls.destroy();

			setTimeout(() => {
				create();
			}, 2000);
		}
	});

	hls.loadSource('stream.m3u8');
	hls.attachMedia(video);

	video.play();
}
create();

</script>

</body>
</html>
`

func ipEqualOrInRange(ip net.IP, ips []interface{}) bool {
	for _, item := range ips {
		switch titem := item.(type) {
		case net.IP:
			if titem.Equal(ip) {
				return true
			}

		case *net.IPNet:
			if titem.Contains(ip) {
				return true
			}
		}
	}
	return false
}

type trackIDPayloadPair struct {
	trackID int
	buf     []byte
}

// PathMan is implemented by pathman.PathMan.
type PathMan interface {
	OnClientSetupPlay(client.SetupPlayReq)
}

// Parent is implemented by clientman.ClientMan.
type Parent interface {
	Log(logger.Level, string, ...interface{})
	OnClientClose(client.Client)
}

// Client is a HLS client.
type Client struct {
	hlsSegmentCount    int
	hlsSegmentDuration time.Duration
	readBufferCount    int
	wg                 *sync.WaitGroup
	stats              *stats.Stats
	pathName           string
	pathMan            PathMan
	parent             Parent

	path            client.Path
	ringBuffer      *ringbuffer.RingBuffer
	tsQueue         []*tsFile
	tsByName        map[string]*tsFile
	tsDeleteCount   int
	tsMutex         sync.Mutex
	lastRequestTime int64

	// in
	request   chan serverhls.Request
	terminate chan struct{}
}

// New allocates a Client.
func New(
	hlsSegmentCount int,
	hlsSegmentDuration time.Duration,
	readBufferCount int,
	wg *sync.WaitGroup,
	stats *stats.Stats,
	pathName string,
	pathMan PathMan,
	parent Parent) *Client {

	c := &Client{
		hlsSegmentCount:    hlsSegmentCount,
		hlsSegmentDuration: hlsSegmentDuration,
		readBufferCount:    readBufferCount,
		wg:                 wg,
		stats:              stats,
		pathName:           pathName,
		pathMan:            pathMan,
		parent:             parent,
		lastRequestTime:    time.Now().Unix(),
		tsByName:           make(map[string]*tsFile),
		request:            make(chan serverhls.Request),
		terminate:          make(chan struct{}),
	}

	atomic.AddInt64(c.stats.CountClients, 1)
	c.log(logger.Info, "connected (HLS)")

	c.wg.Add(1)
	go c.run()

	return c
}

// Close closes a Client.
func (c *Client) Close() {
	atomic.AddInt64(c.stats.CountClients, -1)
	close(c.terminate)
}

// IsClient implements client.Client.
func (c *Client) IsClient() {}

// IsSource implements path.source.
func (c *Client) IsSource() {}

func (c *Client) log(level logger.Level, format string, args ...interface{}) {
	c.parent.Log(level, "[client hls/%s] "+format, append([]interface{}{c.pathName}, args...)...)
}

// PathName returns the path name of the client.
func (c *Client) PathName() string {
	return c.pathName
}

func (c *Client) run() {
	defer c.wg.Done()
	defer c.log(logger.Info, "disconnected")

	var videoTrack *gortsplib.Track
	var h264SPS []byte
	var h264PPS []byte
	var h264Decoder *rtph264.Decoder
	var audioTrack *gortsplib.Track
	var aacConfig rtpaac.MPEG4AudioConfig
	var aacDecoder *rtpaac.Decoder

	err := func() error {
		pres := make(chan client.SetupPlayRes)
		c.pathMan.OnClientSetupPlay(client.SetupPlayReq{c, c.pathName, nil, pres}) //nolint:govet
		res := <-pres

		if res.Err != nil {
			return res.Err
		}

		c.path = res.Path

		for i, t := range res.Tracks {
			if t.IsH264() {
				if videoTrack != nil {
					return fmt.Errorf("can't read track %d with HLS: too many tracks", i+1)
				}
				videoTrack = t

				var err error
				h264SPS, h264PPS, err = t.ExtractDataH264()
				if err != nil {
					return err
				}

				h264Decoder = rtph264.NewDecoder()

			} else if t.IsAAC() {
				if audioTrack != nil {
					return fmt.Errorf("can't read track %d with HLS: too many tracks", i+1)
				}
				audioTrack = t

				byts, err := t.ExtractDataAAC()
				if err != nil {
					return err
				}

				err = aacConfig.Decode(byts)
				if err != nil {
					return err
				}

				aacDecoder = rtpaac.NewDecoder(aacConfig.SampleRate)
			}
		}

		if videoTrack == nil && audioTrack == nil {
			return fmt.Errorf("unable to find a video or audio track")
		}

		return nil
	}()
	if err != nil {
		c.log(logger.Info, "ERR: %s", err)

		go func() {
			for req := range c.request {
				req.W.WriteHeader(http.StatusNotFound)
				req.Res <- nil
			}
		}()

		if c.path != nil {
			res := make(chan struct{})
			c.path.OnClientRemove(client.RemoveReq{c, res}) //nolint:govet
			<-res
		}

		c.parent.OnClientClose(c)
		<-c.terminate

		close(c.request)
		return
	}

	curTSFile := newTSFile(videoTrack, audioTrack)
	c.tsByName[curTSFile.Name()] = curTSFile
	c.tsQueue = append(c.tsQueue, curTSFile)

	defer func() {
		curTSFile.Close()
	}()

	requestDone := make(chan struct{})
	go c.runRequestHandler(requestDone)

	defer func() {
		close(c.request)
		<-requestDone
	}()

	c.ringBuffer = ringbuffer.New(uint64(c.readBufferCount))

	resc := make(chan client.PlayRes)
	c.path.OnClientPlay(client.PlayReq{c, resc}) //nolint:govet
	<-resc

	c.log(logger.Info, "is reading from path '%s'", c.pathName)

	writerDone := make(chan error)
	go func() {
		writerDone <- func() error {
			startPCR := time.Now()
			var videoBuf [][]byte
			videoDTSEst := h264.NewDTSEstimator()
			audioAUCount := 0

			for {
				data, ok := c.ringBuffer.Pull()
				if !ok {
					return fmt.Errorf("terminated")
				}
				pair := data.(trackIDPayloadPair)

				if videoTrack != nil && pair.trackID == videoTrack.ID {
					nalus, pts, err := h264Decoder.Decode(pair.buf)
					if err != nil {
						if err != rtph264.ErrMorePacketsNeeded {
							c.log(logger.Warn, "unable to decode video track: %v", err)
						}
						continue
					}

					for _, nalu := range nalus {
						// remove SPS, PPS, AUD
						typ := h264.NALUType(nalu[0] & 0x1F)
						switch typ {
						case h264.NALUTypeSPS, h264.NALUTypePPS, h264.NALUTypeAccessUnitDelimiter:
							continue
						}

						// add SPS and PPS before IDR
						if typ == h264.NALUTypeIDR {
							videoBuf = append(videoBuf, h264SPS)
							videoBuf = append(videoBuf, h264PPS)
						}

						videoBuf = append(videoBuf, nalu)
					}

					// RTP marker means that all the NALUs with the same PTS have been received.
					// send them together.
					marker := (pair.buf[1] >> 7 & 0x1) > 0
					if marker {
						isIDR := func() bool {
							for _, nalu := range videoBuf {
								typ := h264.NALUType(nalu[0] & 0x1F)
								if typ == h264.NALUTypeIDR {
									return true
								}
							}
							return false
						}()

						if isIDR {
							if curTSFile.firstPacketWritten &&
								time.Since(curTSFile.firstPacketWrittenTime) >= c.hlsSegmentDuration {
								if curTSFile != nil {
									curTSFile.Close()
								}

								curTSFile = newTSFile(videoTrack, audioTrack)
								c.tsMutex.Lock()
								c.tsByName[curTSFile.Name()] = curTSFile
								c.tsQueue = append(c.tsQueue, curTSFile)
								if len(c.tsQueue) > c.hlsSegmentCount {
									delete(c.tsByName, c.tsQueue[0].Name())
									c.tsQueue = c.tsQueue[1:]
									c.tsDeleteCount++
								}
								c.tsMutex.Unlock()
							}

						} else {
							if !curTSFile.firstPacketWritten {
								continue
							}
						}

						curTSFile.SetPCR(time.Since(startPCR))
						err := curTSFile.WriteH264(
							videoDTSEst.Feed(pts+ptsOffset),
							pts+ptsOffset,
							isIDR,
							videoBuf)
						if err != nil {
							return err
						}

						videoBuf = nil
					}

				} else if audioTrack != nil && pair.trackID == audioTrack.ID {
					aus, pts, err := aacDecoder.Decode(pair.buf)
					if err != nil {
						if err != rtpaac.ErrMorePacketsNeeded {
							c.log(logger.Warn, "unable to decode audio track: %v", err)
						}
						continue
					}

					if videoTrack == nil {
						if curTSFile.firstPacketWritten &&
							(time.Since(curTSFile.firstPacketWrittenTime) >= c.hlsSegmentDuration &&
								audioAUCount >= segmentMinAUCount) {

							if curTSFile != nil {
								curTSFile.Close()
							}

							audioAUCount = 0
							curTSFile = newTSFile(videoTrack, audioTrack)
							c.tsMutex.Lock()
							c.tsByName[curTSFile.Name()] = curTSFile
							c.tsQueue = append(c.tsQueue, curTSFile)
							if len(c.tsQueue) > c.hlsSegmentCount {
								delete(c.tsByName, c.tsQueue[0].Name())
								c.tsQueue = c.tsQueue[1:]
								c.tsDeleteCount++
							}
							c.tsMutex.Unlock()
						}
					} else {
						if !curTSFile.firstPacketWritten {
							continue
						}
					}

					for i, au := range aus {
						auPTS := pts + time.Duration(i)*1000*time.Second/time.Duration(aacConfig.SampleRate)

						audioAUCount++
						curTSFile.SetPCR(time.Since(startPCR))
						err := curTSFile.WriteAAC(
							aacConfig.SampleRate,
							aacConfig.ChannelCount,
							auPTS+ptsOffset,
							au)
						if err != nil {
							return err
						}
					}
				}
			}
		}()
	}()

	closeCheckTicker := time.NewTicker(closeCheckPeriod)
	defer closeCheckTicker.Stop()

	for {
		select {
		case <-closeCheckTicker.C:
			t := time.Unix(atomic.LoadInt64(&c.lastRequestTime), 0)
			if time.Since(t) >= closeAfterInactivity {
				c.log(logger.Info, "closing due to inactivity")

				c.ringBuffer.Close()
				<-writerDone

				res := make(chan struct{})
				c.path.OnClientRemove(client.RemoveReq{c, res}) //nolint:govet
				<-res

				c.parent.OnClientClose(c)
				<-c.terminate
				return
			}

		case err := <-writerDone:
			c.log(logger.Info, "ERR: %s", err)

			res := make(chan struct{})
			c.path.OnClientRemove(client.RemoveReq{c, res}) //nolint:govet
			<-res

			c.parent.OnClientClose(c)
			<-c.terminate
			return

		case <-c.terminate:
			res := make(chan struct{})
			c.path.OnClientRemove(client.RemoveReq{c, res}) //nolint:govet
			<-res

			c.ringBuffer.Close()
			<-writerDone
			return
		}
	}
}

func (c *Client) runRequestHandler(done chan struct{}) {
	defer close(done)

	for preq := range c.request {
		req := preq

		atomic.StoreInt64(&c.lastRequestTime, time.Now().Unix())

		conf := c.path.Conf()

		if conf.ReadIpsParsed != nil {
			tmp, _, _ := net.SplitHostPort(req.Req.RemoteAddr)
			ip := net.ParseIP(tmp)
			if !ipEqualOrInRange(ip, conf.ReadIpsParsed) {
				c.log(logger.Info, "ERR: ip '%s' not allowed", ip)
				req.W.WriteHeader(http.StatusUnauthorized)
				req.Res <- nil
				continue
			}
		}

		if conf.ReadUser != "" {
			user, pass, ok := req.Req.BasicAuth()
			if !ok || user != conf.ReadUser || pass != conf.ReadPass {
				req.W.Header().Set("WWW-Authenticate", `Basic realm="rtsp-simple-server"`)
				req.W.WriteHeader(http.StatusUnauthorized)
				req.Res <- nil
				continue
			}
		}

		switch {
		case req.Subpath == "stream.m3u8":
			func() {
				c.tsMutex.Lock()
				defer c.tsMutex.Unlock()

				if len(c.tsQueue) == 0 {
					req.W.WriteHeader(http.StatusNotFound)
					req.Res <- nil
					return
				}

				cnt := "#EXTM3U\n"
				cnt += "#EXT-X-VERSION:3\n"
				cnt += "#EXT-X-ALLOW-CACHE:NO\n"
				cnt += "#EXT-X-TARGETDURATION:10\n"
				cnt += "#EXT-X-MEDIA-SEQUENCE:" + strconv.FormatInt(int64(c.tsDeleteCount), 10) + "\n"
				for _, f := range c.tsQueue {
					cnt += "#EXTINF:10,\n"
					cnt += f.Name() + ".ts\n"
				}
				req.Res <- bytes.NewReader([]byte(cnt))
			}()

		case strings.HasSuffix(req.Subpath, ".ts"):
			base := strings.TrimSuffix(req.Subpath, ".ts")

			c.tsMutex.Lock()
			f, ok := c.tsByName[base]
			c.tsMutex.Unlock()

			if !ok {
				req.W.WriteHeader(http.StatusNotFound)
				req.Res <- nil
				continue
			}

			req.Res <- f.buf.NewReader()

		case req.Subpath == "":
			req.Res <- bytes.NewReader([]byte(index))

		default:
			req.W.WriteHeader(http.StatusNotFound)
			req.Res <- nil
		}
	}
}

// OnRequest is called by clientman.ClientMan.
func (c *Client) OnRequest(req serverhls.Request) {
	c.request <- req
}

// Authenticate performs an authentication.
func (c *Client) Authenticate(authMethods []headers.AuthMethod,
	pathName string, ips []interface{},
	user string, pass string, req interface{}) error {
	return nil
}

// OnFrame implements path.Reader.
func (c *Client) OnFrame(trackID int, streamType gortsplib.StreamType, payload []byte) {
	if streamType == gortsplib.StreamTypeRTP {
		c.ringBuffer.Push(trackIDPayloadPair{trackID, payload})
	}
}
