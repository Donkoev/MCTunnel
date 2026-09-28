// Package kcp is a port of KCP (github.com/skywind3000/kcp, MIT licence), an ARQ protocol that
// trades some bandwidth for latency on lossy links: retransmission after tens of milliseconds
// instead of TCP's hundreds, selective and fast acknowledgements, no delayed ACKs.
//
// The algorithm and the wire format are those of ikcp.c, as in the Java port in the mod; both
// check the same test vector. Two additions, on the sending side only (the wire format does not
// change): DelayControl, a send window that follows the round trip time, and needless resends are
// recognised (the acknowledgement names the copy it confirms) and make the resend timer wait
// longer for a while. Fast resends follow ikcp's IKCP_FASTACK_CONSERVE. The caller feeds received
// datagrams to Input, sends what the output function gets, and calls Update on a timer. Only
// message mode is implemented (every Send is delivered as one message by Recv). Not safe for
// concurrent use.
package kcp

import "encoding/binary"

const (
	rtoNoDelay      = 30 // minimum RTO in no-delay mode (ms)
	rtoMin          = 100
	rtoDefault      = 200
	rtoMax          = 60000
	cmdPush         = 81 // data
	cmdAck          = 82
	cmdWindowAsk    = 83
	cmdWindowTell   = 84
	askSend         = 1
	askTell         = 2
	wndSndDefault   = 32
	wndRcvDefault   = 128 // also the most fragments one message may have
	mtuDefault      = 1400
	intervalDefault = 100
	// Overhead is the size of a segment header.
	Overhead        = 24
	deadLinkDefault = 20
	threshInit      = 2
	threshMin       = 2
	probeInit       = 7000
	probeLimit      = 120000
	fastAckLimit    = 5

	// Delay control (not in ikcp).
	dwndInit       = 16
	dwndMin        = 16
	queueMinMs     = 10
	minRttWindowMs = 30000
)

type segment struct {
	conv     uint32
	cmd      uint8
	frg      uint8
	wnd      uint16
	ts       uint32
	sn       uint32
	una      uint32
	resendts uint32
	rto      uint32
	fastack  uint32
	xmit     uint32
	firstTs  uint32 // when the first copy left: tells a lost segment from a needless resend
	data     []byte
}

type ackItem struct{ sn, ts uint32 }

// KCP is one conversation, identified by conv on both sides.
type KCP struct {
	conv, mtu, mss, state      uint32
	sndUna, sndNxt, rcvNxt     uint32
	ssthresh                   uint32
	rxRttval, rxSrtt           int32
	rxRto, rxMinrto            int32
	sndWnd, rcvWnd, rmtWnd     uint32
	cwnd, probe                uint32
	current, interval, tsFlush uint32
	xmit                       uint32
	nodelay                    int32
	updated                    bool
	tsProbe, probeWait         uint32
	deadLink, incr             uint32
	fastresend, fastlimit      int32
	nocwnd                     bool

	sndQueue []*segment
	rcvQueue []*segment
	sndBuf   []*segment
	rcvBuf   []*segment
	acklist  []ackItem

	buffer []byte
	output func([]byte)

	sentSegments, resentSegments, lostSegments uint64

	rtoFloor     uint32 // raised by a needless resend, halves in about half a second
	delayControl bool
	dwnd         uint32
	startup      bool  // doubling per round until the first queue shows
	minRtt       int32 // lowest round trip lately, ms (-1: none yet)
	minRttAt     uint32
	roundEnd     uint32 // this round ends once everything sent before it is acknowledged
	roundSum     int64  // round trips sampled this round
	roundSamples int
	roundMin     int32 // lowest round trip of this round (-1: none yet)
	roundLost    int   // segments found lost this round
	roundAcked   int
	roundLimited bool // this round, data had to wait for the window
}

// New creates a conversation. output is called with every datagram to send; the slice is reused
// after the call returns.
func New(conv uint32, output func([]byte)) *KCP {
	k := &KCP{
		conv: conv, mtu: mtuDefault, sndWnd: wndSndDefault, rcvWnd: wndRcvDefault, rmtWnd: wndRcvDefault,
		rxRto: rtoDefault, rxMinrto: rtoMin, interval: intervalDefault, tsFlush: intervalDefault,
		ssthresh: threshInit, fastlimit: fastAckLimit, deadLink: deadLinkDefault, output: output,
		dwnd: dwndInit, startup: true, minRtt: -1, roundMin: -1,
	}
	k.mss = k.mtu - Overhead
	k.buffer = make([]byte, 0, (k.mtu+Overhead)*3)
	return k
}

func diff(later, earlier uint32) int32 { return int32(later - earlier) }

// NoDelay tunes the protocol: nodelay (1 = aggressive RTO), the update interval in ms, fast
// resend after this many duplicate ACKs (0 = off) and nc (true = no congestion window).
func (k *KCP) NoDelay(nodelay, interval, resend int, nc bool) {
	k.nodelay = int32(nodelay)
	if nodelay != 0 {
		k.rxMinrto = rtoNoDelay
	} else {
		k.rxMinrto = rtoMin
	}
	interval = min(max(interval, 10), 5000)
	k.interval = uint32(interval)
	k.fastresend = int32(resend)
	k.nocwnd = nc
}

// DelayControl limits the data not yet acknowledged by the round trip time, like TCP Vegas: while
// round trips stay near the lowest seen, the window grows (doubling at first, like TCP's slow
// start); once they exceed it by a quarter (at least 10 ms), a queue is building at the
// bottleneck and the window shrinks (see endRound). A fixed window without it (ikcp with nc) fills
// the router's buffer on every burst: the latency of everything else on the line jumps, and
// resend timers that no longer match the round trip send much of the burst twice for nothing.
func (k *KCP) DelayControl(on bool) { k.delayControl = on }

// DelayWindow is the delay-controlled window in segments.
func (k *KCP) DelayWindow() int { return int(k.dwnd) }

// SentSegments counts data segments sent for the first time.
func (k *KCP) SentSegments() uint64 { return k.sentSegments }

// ResentSegments counts data segments sent again (timeout or fast resend), needed or not.
func (k *KCP) ResentSegments() uint64 { return k.resentSegments }

// LostSegments counts data segments whose first copy (or its acknowledgement) was lost: the
// acknowledgement that finally came is for a later copy. A resend that only raced a slow
// acknowledgement does not count.
func (k *KCP) LostSegments() uint64 { return k.lostSegments }

// WndSize sets the send and receive windows (in segments).
func (k *KCP) WndSize(snd, rcv int) {
	if snd > 0 {
		k.sndWnd = uint32(snd)
	}
	if rcv > 0 {
		k.rcvWnd = uint32(max(rcv, wndRcvDefault))
	}
}

// SetMtu sets the largest datagram; returns false if it is too small.
func (k *KCP) SetMtu(mtu int) bool {
	if mtu < 50 || mtu < Overhead {
		return false
	}
	k.mtu = uint32(mtu)
	k.mss = k.mtu - Overhead
	k.buffer = make([]byte, 0, (k.mtu+Overhead)*3)
	return true
}

// WaitSnd is the number of segments not yet acknowledged or not yet sent.
func (k *KCP) WaitSnd() int { return len(k.sndBuf) + len(k.sndQueue) }

// Dead reports a segment retransmitted too often: the peer is gone.
func (k *KCP) Dead() bool { return k.state == 0xFFFFFFFF }

// PeekSize is the size of the next complete message, or -1.
func (k *KCP) PeekSize() int {
	if len(k.rcvQueue) == 0 {
		return -1
	}
	seg := k.rcvQueue[0]
	if seg.frg == 0 {
		return len(seg.data)
	}
	if len(k.rcvQueue) < int(seg.frg)+1 {
		return -1
	}
	length := 0
	for _, s := range k.rcvQueue {
		length += len(s.data)
		if s.frg == 0 {
			break
		}
	}
	return length
}

// Recv takes the next message into buf and returns its length: -1 = none, -2 = incomplete,
// -3 = buf too small.
func (k *KCP) Recv(buf []byte) int {
	if len(k.rcvQueue) == 0 {
		return -1
	}
	peek := k.PeekSize()
	if peek < 0 {
		return -2
	}
	if peek > len(buf) {
		return -3
	}
	recover := uint32(len(k.rcvQueue)) >= k.rcvWnd
	n, count := 0, 0
	for _, seg := range k.rcvQueue {
		copy(buf[n:], seg.data)
		n += len(seg.data)
		count++
		if seg.frg == 0 {
			break
		}
	}
	k.rcvQueue = append(k.rcvQueue[:0], k.rcvQueue[count:]...)
	k.moveReady()
	if uint32(len(k.rcvQueue)) < k.rcvWnd && recover {
		k.probe |= askTell // the window reopened: tell the peer
	}
	return n
}

// moveReady moves in-order segments from the receive buffer to the receive queue.
func (k *KCP) moveReady() {
	i := 0
	for i < len(k.rcvBuf) {
		seg := k.rcvBuf[i]
		if seg.sn != k.rcvNxt || uint32(len(k.rcvQueue)) >= k.rcvWnd {
			break
		}
		k.rcvQueue = append(k.rcvQueue, seg)
		k.rcvNxt++
		i++
	}
	if i > 0 {
		k.rcvBuf = append(k.rcvBuf[:0], k.rcvBuf[i:]...)
	}
}

// Send queues one message; returns false if it needs too many fragments.
func (k *KCP) Send(buf []byte) bool {
	count := 1
	if len(buf) > int(k.mss) {
		count = (len(buf) + int(k.mss) - 1) / int(k.mss)
	}
	if count >= wndRcvDefault {
		return false
	}
	for i := 0; i < count; i++ {
		size := min(len(buf), int(k.mss))
		seg := &segment{data: append([]byte(nil), buf[:size]...), frg: uint8(count - i - 1)}
		k.sndQueue = append(k.sndQueue, seg)
		buf = buf[size:]
	}
	return true
}

func (k *KCP) updateAck(rtt int32) {
	if k.rxSrtt == 0 {
		k.rxSrtt = rtt
		k.rxRttval = rtt / 2
	} else {
		delta := rtt - k.rxSrtt
		if delta < 0 {
			delta = -delta
		}
		k.rxRttval = (3*k.rxRttval + delta) / 4
		k.rxSrtt = (7*k.rxSrtt + rtt) / 8
		if k.rxSrtt < 1 {
			k.rxSrtt = 1
		}
	}
	rto := k.rxSrtt + max(int32(k.interval), 4*k.rxRttval)
	k.rxRto = min(max(k.rxMinrto, rto), rtoMax)
}

func (k *KCP) shrinkBuf() {
	if len(k.sndBuf) > 0 {
		k.sndUna = k.sndBuf[0].sn
	} else {
		k.sndUna = k.sndNxt
	}
}

// accountAck does the loss accounting for the segment an acknowledgement names, before its una
// field removes it: a resent segment was lost if the acknowledged copy is not the first one.
func (k *KCP) accountAck(sn, ts uint32) {
	if diff(sn, k.sndUna) < 0 || diff(sn, k.sndNxt) >= 0 {
		return
	}
	for _, seg := range k.sndBuf {
		if sn == seg.sn {
			if seg.xmit > 1 {
				if ts != seg.firstTs {
					k.lostSegments++
					k.roundLost++
				} else {
					k.needlessResend(diff(k.current, seg.firstTs))
				}
			}
			return
		}
		if diff(sn, seg.sn) < 0 {
			return
		}
	}
}

// needlessResend: the first copy of a resent segment is being acknowledged, it was not lost,
// only slower than the resend timer, typically stuck in a queue behind a burst. Resending now
// would add to that queue and cascade (every segment of the burst sent twice), so the timer waits
// at least this long for a while, also for the segments already on their way. Real losses in a
// burst are still resent quickly: by fast resend.
func (k *KCP) needlessResend(rtt int32) {
	floor := min(rtoMax, max(int32(k.rtoFloor), rtt+max(int32(k.interval), rtt/4)))
	k.rtoFloor = uint32(floor)
	for _, s := range k.sndBuf {
		if diff(s.resendts, s.ts+k.rtoFloor) < 0 {
			s.resendts = s.ts + k.rtoFloor
			s.rto = max(s.rto, k.rtoFloor)
		}
	}
}

func (k *KCP) rto() uint32 { return uint32(max(k.rxRto, int32(k.rtoFloor))) }

func (k *KCP) sampleRtt(rtt int32) {
	if k.minRtt < 0 || rtt <= k.minRtt || diff(k.current, k.minRttAt) > minRttWindowMs {
		k.minRtt = rtt
		k.minRttAt = k.current
	}
	k.roundSum += int64(rtt)
	k.roundSamples++
	if k.roundMin < 0 || rtt < k.roundMin {
		k.roundMin = rtt
	}
}

// endRound runs once a round trip. A queue that stands shows in the round's lowest round trip
// (jitter only ever adds to it); the queue a burst builds shows in the average. Either one above
// the budget (twice the budget for the average, which jitter also lifts): the window shrinks in
// proportion, at least by an eighth. Neither: a window that was the limit doubles at first (like
// TCP's slow start), later grows by a quarter; with a little queue in the average, by one
// segment. A burst of losses shrinks it by a quarter: a buffer on the way too small to show a
// queue first.
func (k *KCP) endRound() {
	if k.roundSamples > 0 && k.minRtt >= 0 {
		budget := max(queueMinMs, k.minRtt/4)
		standing := k.roundMin - k.minRtt
		mean := int32(k.roundSum/int64(k.roundSamples)) - k.minRtt
		limit := max(k.sndWnd, dwndMin)
		switch {
		case k.roundLost >= 3 && k.roundLost*20 > k.roundAcked:
			k.dwnd = max(dwndMin, k.dwnd-k.dwnd/4)
			k.startup = false
		case standing > budget || mean > 2*budget:
			target := uint32(int64(k.dwnd) * int64(k.minRtt+budget/2) / int64(max(1, k.minRtt+max(standing, mean/2))))
			k.dwnd = max(dwndMin, min(k.dwnd-k.dwnd/8, target))
			k.startup = false
		case k.roundLimited && standing <= budget/2 && mean <= budget:
			if k.startup {
				k.dwnd = min(limit, k.dwnd*2)
			} else {
				k.dwnd = min(limit, k.dwnd+max(1, k.dwnd/4))
			}
		case k.roundLimited && standing <= budget/2:
			k.dwnd = min(limit, k.dwnd+1)
		}
	}
	k.roundEnd = k.sndNxt
	k.roundSum = 0
	k.roundSamples = 0
	k.roundMin = -1
	k.roundLost = 0
	k.roundAcked = 0
	k.roundLimited = false
}

func (k *KCP) parseAck(sn uint32) {
	if diff(sn, k.sndUna) < 0 || diff(sn, k.sndNxt) >= 0 {
		return
	}
	for i, seg := range k.sndBuf {
		if sn == seg.sn {
			k.sndBuf = append(k.sndBuf[:i], k.sndBuf[i+1:]...)
			break
		}
		if diff(sn, seg.sn) < 0 {
			break
		}
	}
}

func (k *KCP) parseUna(una uint32) {
	i := 0
	for i < len(k.sndBuf) && diff(una, k.sndBuf[i].sn) > 0 {
		i++
	}
	if i > 0 {
		k.sndBuf = append(k.sndBuf[:0], k.sndBuf[i:]...)
	}
}

// parseFastack: segments older than the newest acknowledged one were probably lost. Only
// acknowledgements of segments sent after a segment's latest copy count (ikcp's
// IKCP_FASTACK_CONSERVE): otherwise a lost segment is resent again every two acknowledgements
// until the first resend's own acknowledgement can arrive.
func (k *KCP) parseFastack(sn, ts uint32) {
	if diff(sn, k.sndUna) < 0 || diff(sn, k.sndNxt) >= 0 {
		return
	}
	for _, seg := range k.sndBuf {
		if diff(sn, seg.sn) < 0 {
			break
		} else if sn != seg.sn && diff(ts, seg.ts) >= 0 {
			seg.fastack++
		}
	}
}

func (k *KCP) parseData(newseg *segment) {
	sn := newseg.sn
	if diff(sn, k.rcvNxt+k.rcvWnd) >= 0 || diff(sn, k.rcvNxt) < 0 {
		return
	}
	insertAt := 0
	repeat := false
	for i := len(k.rcvBuf) - 1; i >= 0; i-- {
		seg := k.rcvBuf[i]
		if seg.sn == sn {
			repeat = true
			break
		}
		if diff(sn, seg.sn) > 0 {
			insertAt = i + 1
			break
		}
	}
	if !repeat {
		k.rcvBuf = append(k.rcvBuf, nil)
		copy(k.rcvBuf[insertAt+1:], k.rcvBuf[insertAt:])
		k.rcvBuf[insertAt] = newseg
	}
	k.moveReady()
}

// Input takes one received datagram (one or more segments). current is the clock in ms. Returns
// 0, or a negative number for a datagram that is not for this conversation or malformed. A
// segment longer than the MSS, or a message with more fragments than the receive window holds,
// is malformed: the peer's Send never makes one (both sides must use the same MTU), and the
// receive limits count segments, so oversized ones would let a peer pin far more memory.
func (k *KCP) Input(data []byte, current uint32) int {
	k.current = current
	prevUna := k.sndUna
	inFlightBefore := len(k.sndBuf)
	var maxack, latestTs uint32
	flag := false
	if len(data) < Overhead {
		return -1
	}
	for len(data) >= Overhead {
		conv := binary.LittleEndian.Uint32(data)
		if conv != k.conv {
			return -1
		}
		cmd := data[4]
		frg := data[5]
		wnd := binary.LittleEndian.Uint16(data[6:])
		ts := binary.LittleEndian.Uint32(data[8:])
		sn := binary.LittleEndian.Uint32(data[12:])
		una := binary.LittleEndian.Uint32(data[16:])
		length := binary.LittleEndian.Uint32(data[20:])
		data = data[Overhead:]
		if uint32(len(data)) < length || int32(length) < 0 {
			return -2
		}
		if length > k.mss || (cmd == cmdPush && uint32(frg) >= k.rcvWnd) {
			return -2
		}
		if cmd != cmdPush && cmd != cmdAck && cmd != cmdWindowAsk && cmd != cmdWindowTell {
			return -3
		}
		k.rmtWnd = uint32(wnd)
		if cmd == cmdAck {
			k.accountAck(sn, ts)
		}
		k.parseUna(una)
		k.shrinkBuf()
		switch cmd {
		case cmdAck:
			if diff(k.current, ts) >= 0 {
				k.updateAck(diff(k.current, ts))
				k.sampleRtt(diff(k.current, ts))
			}
			k.parseAck(sn)
			k.shrinkBuf()
			if !flag {
				flag = true
				maxack, latestTs = sn, ts
			} else if diff(sn, maxack) > 0 && diff(ts, latestTs) > 0 {
				maxack, latestTs = sn, ts
			}
		case cmdPush:
			if diff(sn, k.rcvNxt+k.rcvWnd) < 0 {
				k.acklist = append(k.acklist, ackItem{sn, ts})
				if diff(sn, k.rcvNxt) >= 0 {
					seg := &segment{conv: conv, cmd: cmd, frg: frg, wnd: wnd, ts: ts, sn: sn, una: una,
						data: append([]byte(nil), data[:length]...)}
					k.parseData(seg)
				}
			}
		case cmdWindowAsk:
			k.probe |= askTell
		case cmdWindowTell:
			// nothing: rmtWnd is already updated
		}
		data = data[length:]
	}
	if flag {
		k.parseFastack(maxack, latestTs)
	}
	k.roundAcked += max(0, inFlightBefore-len(k.sndBuf))
	if k.delayControl && diff(k.sndUna, k.roundEnd) >= 0 {
		k.endRound()
	}
	if diff(k.sndUna, prevUna) > 0 && k.cwnd < k.rmtWnd {
		mss := k.mss
		if k.cwnd < k.ssthresh {
			k.cwnd++
			k.incr += mss
		} else {
			if k.incr < mss {
				k.incr = mss
			}
			k.incr += (mss*mss)/k.incr + mss/16
			if (k.cwnd+1)*mss <= k.incr {
				k.cwnd = (k.incr + mss - 1) / mss
			}
		}
		if k.cwnd > k.rmtWnd {
			k.cwnd = k.rmtWnd
			k.incr = k.rmtWnd * mss
		}
	}
	return 0
}

func encodeSeg(buf []byte, seg *segment) []byte {
	var h [Overhead]byte
	binary.LittleEndian.PutUint32(h[0:], seg.conv)
	h[4] = seg.cmd
	h[5] = seg.frg
	binary.LittleEndian.PutUint16(h[6:], seg.wnd)
	binary.LittleEndian.PutUint32(h[8:], seg.ts)
	binary.LittleEndian.PutUint32(h[12:], seg.sn)
	binary.LittleEndian.PutUint32(h[16:], seg.una)
	binary.LittleEndian.PutUint32(h[20:], uint32(len(seg.data)))
	return append(buf, h[:]...)
}

func (k *KCP) wndUnused() uint16 {
	if uint32(len(k.rcvQueue)) < k.rcvWnd {
		return uint16(k.rcvWnd - uint32(len(k.rcvQueue)))
	}
	return 0
}

// Flush sends pending ACKs, window probes, new data and due retransmissions now. current is the
// clock in ms. Update calls it on its schedule; callers call it after Send and Input for the
// lowest latency.
func (k *KCP) Flush(current uint32) {
	k.current = current
	if !k.updated {
		return
	}
	buf := k.buffer[:0]
	emit := func(need int) {
		if len(buf)+need > int(k.mtu) && len(buf) > 0 {
			k.output(buf)
			buf = buf[:0]
		}
	}
	seg := segment{conv: k.conv, cmd: cmdAck, wnd: k.wndUnused(), una: k.rcvNxt}

	for _, ack := range k.acklist {
		emit(Overhead)
		seg.sn, seg.ts = ack.sn, ack.ts
		buf = encodeSeg(buf, &seg)
	}
	k.acklist = k.acklist[:0]

	// Probe the window if the peer's is closed.
	if k.rmtWnd == 0 {
		if k.probeWait == 0 {
			k.probeWait = probeInit
			k.tsProbe = k.current + k.probeWait
		} else if diff(k.current, k.tsProbe) >= 0 {
			if k.probeWait < probeInit {
				k.probeWait = probeInit
			}
			k.probeWait += k.probeWait / 2
			if k.probeWait > probeLimit {
				k.probeWait = probeLimit
			}
			k.tsProbe = k.current + k.probeWait
			k.probe |= askSend
		}
	} else {
		k.tsProbe = 0
		k.probeWait = 0
	}
	seg.sn, seg.ts = 0, 0
	if k.probe&askSend != 0 {
		seg.cmd = cmdWindowAsk
		emit(Overhead)
		buf = encodeSeg(buf, &seg)
	}
	if k.probe&askTell != 0 {
		seg.cmd = cmdWindowTell
		emit(Overhead)
		buf = encodeSeg(buf, &seg)
	}
	k.probe = 0

	cwnd := min(k.sndWnd, k.rmtWnd)
	if !k.nocwnd {
		cwnd = min(k.cwnd, cwnd)
	}
	// Move data from the send queue into the send buffer, as far as the window allows. The delay
	// window counts the segments not yet acknowledged, not the span from the oldest one: while a
	// lost segment is being resent, the rest keeps flowing.
	moved := 0
	for moved < len(k.sndQueue) && diff(k.sndNxt, k.sndUna+cwnd) < 0 &&
		(!k.delayControl || uint32(len(k.sndBuf)) < k.dwnd) {
		newseg := k.sndQueue[moved]
		newseg.conv = k.conv
		newseg.cmd = cmdPush
		newseg.wnd = seg.wnd
		newseg.ts = current
		newseg.sn = k.sndNxt
		k.sndNxt++
		newseg.una = k.rcvNxt
		newseg.resendts = current
		newseg.rto = k.rto()
		newseg.fastack = 0
		newseg.xmit = 0
		k.sndBuf = append(k.sndBuf, newseg)
		moved++
	}
	if moved > 0 {
		k.sndQueue = append(k.sndQueue[:0], k.sndQueue[moved:]...)
	}
	if len(k.sndQueue) > 0 {
		k.roundLimited = true
	}

	resent := uint32(0xFFFFFFFF)
	if k.fastresend > 0 {
		resent = uint32(k.fastresend)
	}
	var rtomin uint32
	if k.nodelay == 0 {
		rtomin = uint32(k.rxRto >> 3)
	}
	change, lost := false, false
	for _, segment := range k.sndBuf {
		needsend := false
		if segment.xmit == 0 {
			needsend = true
			segment.xmit++
			segment.firstTs = current
			k.sentSegments++
			segment.rto = k.rto()
			segment.resendts = current + segment.rto + rtomin
		} else if diff(current, segment.resendts) >= 0 {
			needsend = true
			segment.xmit++
			k.xmit++
			if k.nodelay == 0 {
				segment.rto += max(segment.rto, uint32(k.rxRto))
			} else {
				step := int32(segment.rto)
				if k.nodelay >= 2 {
					step = k.rxRto
				}
				segment.rto += uint32(step / 2)
			}
			segment.resendts = current + segment.rto
			lost = true
			k.resentSegments++
		} else if segment.fastack >= resent {
			if int32(segment.xmit) <= k.fastlimit || k.fastlimit <= 0 {
				needsend = true
				segment.xmit++
				segment.fastack = 0
				segment.resendts = current + segment.rto
				change = true
				k.resentSegments++
			}
		}
		if needsend {
			segment.ts = current
			segment.wnd = seg.wnd
			segment.una = k.rcvNxt
			emit(Overhead + len(segment.data))
			buf = encodeSeg(buf, segment)
			buf = append(buf, segment.data...)
			if segment.xmit >= k.deadLink {
				k.state = 0xFFFFFFFF
			}
		}
	}
	if len(buf) > 0 {
		k.output(buf)
	}
	k.buffer = buf[:0]

	if change {
		inflight := k.sndNxt - k.sndUna
		k.ssthresh = max(inflight/2, threshMin)
		k.cwnd = k.ssthresh + resent
		k.incr = k.cwnd * k.mss
	}
	if lost {
		k.ssthresh = max(cwnd/2, threshMin)
		k.cwnd = 1
		k.incr = k.mss
	}
	if k.cwnd < 1 {
		k.cwnd = 1
		k.incr = k.mss
	}
}

// Update advances the clock (ms) and flushes when the interval has passed.
func (k *KCP) Update(current uint32) {
	k.current = current
	if !k.updated {
		k.updated = true
		k.tsFlush = current
	}
	slap := diff(current, k.tsFlush)
	if slap >= 10000 || slap < -10000 {
		k.tsFlush = current
		slap = 0
	}
	if slap >= 0 {
		k.rtoFloor -= k.rtoFloor >> 6 // fades: halves in about 0.45 s at 10 ms
		k.tsFlush += k.interval
		if diff(current, k.tsFlush) >= 0 {
			k.tsFlush = current + k.interval
		}
		k.Flush(current)
	}
}

// Check returns when Update should run next (ms clock).
func (k *KCP) Check(current uint32) uint32 {
	if !k.updated {
		return current
	}
	tsFlush := k.tsFlush
	if d := diff(current, tsFlush); d >= 10000 || d < -10000 {
		tsFlush = current
	}
	if diff(current, tsFlush) >= 0 {
		return current
	}
	tmFlush := diff(tsFlush, current)
	tmPacket := int32(0x7FFFFFFF)
	for _, seg := range k.sndBuf {
		d := diff(seg.resendts, current)
		if d <= 0 {
			return current
		}
		tmPacket = min(tmPacket, d)
	}
	minimal := uint32(min(tmPacket, tmFlush))
	if minimal >= k.interval {
		minimal = k.interval
	}
	return current + minimal
}
