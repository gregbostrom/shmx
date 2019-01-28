// Package shmx implements a shared memory (shm) cross (x) interface (shmx).
package shmx

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/sys/unix"
)

// Control the shared memory cross interface (shmx) as either a Master or Slave.
type Control int

const (
	// Master controls, creates and initializes, the shmx.
	Master Control = 1
	// Slave depends on the Master to create the shmx.
	Slave Control = 2
)

const (
	majorVersion = 1
	minorVersion = 0
)

const shmxFlagInit = 1

type shmxConfig struct {
	Major      byte
	Minor      byte
	RingPairs  byte
	_          byte
	RingOffset uint32
	RingStride uint32
	Flags      uint32
}

const shmxConfigSize = 16
const offsetConfigFlags = 12

type cbheader struct {
	ConstSize uint32
	_         uint32
	WIndex    uint32
	WPktWrote uint32
	WPktLost  uint32
	_         uint32
	RIndex    uint32
	RPktRead  uint32
}

// Stats of packets read, packets written, and packets lost by overrun.
type Stats struct {
	RPktRead  uint32
	WPktWrote uint32
	WPktLost  uint32
}

const cbheaderSize uint32 = 32
const offsetConstS uint32 = 0
const offsetWIndex uint32 = 8
const offsetWPktWr uint32 = 12
const offsetWPktLo uint32 = 16
const offsetRIndex uint32 = 24
const offsetRPktRe uint32 = 28

// Shmx is the Shared Memory Cross interface control block.
//   rx = read cross, tx = transmit cross
type Shmx struct {
	role         Control
	path         string
	fd           int
	m            []byte
	size         int
	ready        bool
	rxCbOffset   uint32
	rxOffsetBase uint32
	rxOffsetWrap uint32
	rxRIndexWrap uint32
	txCbOffset   uint32
	txOffsetBase uint32
	txOffsetWrap uint32
	txWIndexWrap uint32
	rx           cbheader
	tx           cbheader
	r            io.Reader
	w            io.Writer
}

type pHeader struct {
	len uint32
	tag uint32
	rd  uint32
}

const pHeaderSize = 12

// ShmxMaxLen is the maximum transfer size.
const ShmxMaxLen = (65535 + 18) // 18 = (ethernet + vlan tag).

func version(major byte, minor byte) string {
	return fmt.Sprintf("%d.%d", major, minor)
}

func (sm *Shmx) reset() {
	if sm.fd != 0 {
		err := unix.Munmap(sm.m)
		if err != nil {
			fmt.Println("Munmap failed: ", err)
		}
		err = unix.Close(sm.fd)
		if err != nil {
			fmt.Println("close failed: ", sm.path, err)
		}
	}

	sm.role = 0
	sm.fd = 0
	sm.m = nil
	sm.path = ""
	sm.ready = false
	return
}

// Stats are the current shmx statistics.
func (sm *Shmx) Stats(s *Stats) {
	if !sm.ready {
		s.RPktRead = 0
		s.WPktWrote = 0
		s.WPktLost = 0
		return
	}

	s.RPktRead = sm.rx.RPktRead
	s.WPktWrote = sm.tx.WPktWrote
	s.WPktLost = sm.tx.WPktLost
}

// Attach to the shared memory as either the Master or Slave.
func (sm *Shmx) Attach(role Control, path string) error {

	var err error

	if sm.role != 0 {
		return errors.New("Inuse")
	}

	sm.role = role
	sm.path = path

	switch role {
	case Master:
		err = sm.createMaster()
	case Slave:
		err = sm.createSlave()
	default:
		err = errors.New("invalid role")
	}

	if err != nil {
		sm.reset()
	}

	return err
}

func (sm *Shmx) createMaster() error {

	var err error

	sm.fd, err = unix.Open(sm.path, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR, unix.S_IRUSR|unix.S_IWUSR)
	if err != nil {
		return fmt.Errorf("open failed %s: %v", sm.path, err)
	}

	const (
		masterRingPairs = 1
		masterRingSize  = 12 * 1024 * 1024
	)

	var tcb shmxConfig

	tcb.Major = 1
	tcb.Minor = 0
	tcb.RingPairs = masterRingPairs
	tcb.RingOffset = shmxConfigSize
	tcb.RingStride = masterRingSize
	tcb.Flags = 0

	sm.size = shmxConfigSize + (masterRingSize * masterRingPairs * 2)

	err = unix.Ftruncate(sm.fd, int64(sm.size))
	if err != nil {
		return fmt.Errorf("ftruncate failed %s: %v ", sm.path, err)
	}

	sm.m, err = unix.Mmap(sm.fd, 0, sm.size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return fmt.Errorf("unix.Mmap failed %s: %v ", sm.path, err)
	}

	buf := new(bytes.Buffer)
	err = binary.Write(buf, binary.LittleEndian, &tcb)
	if err != nil {
		return fmt.Errorf("binary.Write failed %s: %v ", sm.path, err)
	}

	n := copy(sm.m, buf.Bytes())
	if n != shmxConfigSize {
		return fmt.Errorf("Init configBlock mishap %d:%d", n, shmxConfigSize)
	}
	err = unix.Msync(sm.m, unix.MS_SYNC)
	if err != nil {
		return fmt.Errorf("unix.Msync failed: %v ", err)
	}

	// Initialize each ring to an empty state.  Two rings per pair.
	i := shmxConfigSize
	for r := 0; r < (masterRingPairs * 2); r++ {
		err = sm.initRing(sm.m[i:], masterRingSize)
		if err != nil {
			return fmt.Errorf("sm.initRing failed %s: %v ", sm.path, err)
		}
		i += masterRingSize
	}

	// Note the reversal between master and slave.
	sm.rxCbOffset = tcb.RingOffset
	sm.txCbOffset = tcb.RingOffset + tcb.RingStride
	sm.initOffsets(tcb.RingStride)
	sm.getConstSize()

	sm.ready = true
	binary.LittleEndian.PutUint32(sm.m[offsetConfigFlags:], uint32(shmxFlagInit))

	return nil
}

func (sm *Shmx) initOffsets(ringStride uint32) {
	sm.rxOffsetBase = sm.rxCbOffset + cbheaderSize
	sm.txOffsetBase = sm.txCbOffset + cbheaderSize
	sm.rxOffsetWrap = sm.rxCbOffset + ringStride
	sm.txOffsetWrap = sm.txCbOffset + ringStride
	sm.rxRIndexWrap = sm.rxOffsetWrap - sm.rxOffsetBase
	sm.txWIndexWrap = sm.txOffsetWrap - sm.txOffsetBase
}

func (sm *Shmx) createSlave() error {

	var err error

	sm.fd, err = unix.Open(sm.path, unix.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open failed %s: %v", sm.path, err)
	}

	b := make([]byte, shmxConfigSize)

	n, err := unix.Read(sm.fd, b)
	if err != nil {
		return fmt.Errorf("read failed %s: %v", sm.path, err)
	}

	if n != shmxConfigSize {
		return fmt.Errorf("Init configBlock mishap, read short  %d:%d", n, shmxConfigSize)
	}

	buf := bytes.NewBuffer(b)
	var tcb shmxConfig
	err = binary.Read(buf, binary.LittleEndian, &tcb)
	if err != nil {
		return fmt.Errorf("binary.Read failed %s: %v ", sm.path, err)
	}

	fmt.Printf("Slave Version:     %s\n", version(tcb.Major, tcb.Minor))
	fmt.Printf("Slave Ring Pairs:  %d\n", tcb.RingPairs)
	fmt.Printf("Slave ring_offset: %d\n", tcb.RingOffset)
	fmt.Printf("Slave ring_stride: %d\n", tcb.RingStride)
	fmt.Printf("Slave flags:       %d\n", tcb.Flags)

	if tcb.Major != majorVersion || tcb.Minor != minorVersion {
		return errors.New("Unexpected version")
	}
	sm.size = int(tcb.RingOffset) + (int(tcb.RingStride) * int(tcb.RingPairs) * 2)

	fmt.Println("Slave Total size: ", sm.size)

	sm.m, err = unix.Mmap(sm.fd, 0, sm.size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return fmt.Errorf("binary.Mmap failed %s: %v ", sm.path, err)
	}

	// Note the reversal between master and slave.
	sm.txCbOffset = tcb.RingOffset
	sm.rxCbOffset = tcb.RingOffset + tcb.RingStride
	sm.initOffsets(tcb.RingStride)

	if binary.LittleEndian.Uint32(sm.m[offsetConfigFlags:]) != shmxFlagInit {
		sm.Detach()
		return fmt.Errorf("Flags not shmxFlagInit")
	}

	sm.getConstSize()

	fmt.Printf("Slave tx offset: %d\n", sm.txOffsetBase)
	fmt.Printf("Slave rx offset: %d\n", sm.rxOffsetBase)
	sm.ready = true
	return nil
}

func (sm *Shmx) initRing(ring []byte, size uint32) error {

	var err error

	hdr := cbheader{}
	hdr.ConstSize = size - cbheaderSize
	hdr.WIndex = 0
	hdr.WPktWrote = 0
	hdr.WPktLost = 0
	hdr.RIndex = 0
	hdr.RPktRead = 0
	buf := new(bytes.Buffer)
	err = binary.Write(buf, binary.LittleEndian, &hdr)
	if err != nil {
		return fmt.Errorf("binary.Write failed %s: %v ", sm.path, err)
	}

	n := copy(ring, buf.Bytes())
	if n != int(cbheaderSize) {
		return errors.New("initRing mishap")
	}
	return nil
}

// Detach from the shared memory.
func (sm *Shmx) Detach() {
	if sm.role == Master && sm.path != "" {
		err := unix.Unlink(sm.path)
		if err != nil {
			fmt.Println("Unlink failed: ", sm.path, err)
		}
	}
	sm.reset()
	return
}

func round32(n int) int {
	return (n + 3) & ^3
}

func (sm *Shmx) Write(p []byte) (n int, err error) {

	if !sm.ready {
		return 0, fmt.Errorf("Not Initialized")
	}

	if len(p) == 0 {
		return 0, nil
	}

	if len(p) > ShmxMaxLen {
		sm.tx.WPktLost++
		return 0, fmt.Errorf("Too Big")
	}

	sm.refreshTxCB()

	var space int

	// Calculate free space for transmitting.
	if sm.tx.WIndex >= sm.tx.RIndex {
		space = int(sm.tx.ConstSize - (sm.tx.WIndex - sm.tx.RIndex))
	} else {
		space = int(sm.tx.RIndex - sm.tx.WIndex)
	}

	// Enough space for: packet header and the data
	if space < pHeaderSize+round32(len(p)) {
		sm.tx.WPktLost++
		sm.putTxCB()
		return 0, nil
	}

	pHdr := pHeader{}
	pHdr.len = uint32(len(p))
	pHdr.tag = 0
	pHdr.rd = 0
	buf := new(bytes.Buffer)
	err = binary.Write(buf, binary.LittleEndian, &pHdr)
	if err != nil {
		return 0, fmt.Errorf("binary.Write pHdr failed %s: %v ", sm.path, err)
	}

	sm.put(buf.Bytes())
	sm.put(p)

	sm.tx.WIndex = uint32(round32(int(sm.tx.WIndex)))
	sm.tx.WPktWrote++
	sm.putTxCB()

	return len(p), nil
}

func (sm *Shmx) put(b []byte) {
	i := int(sm.txOffsetBase + sm.tx.WIndex)
	n := copy(sm.m[i:sm.txOffsetWrap], b)
	if n == len(b) {
		// got it all
		sm.tx.WIndex += uint32(n)
		return
	}

	// Wrap for the rest.
	m := copy(sm.m[sm.txOffsetBase:sm.txOffsetWrap], b[n:])
	if n+m != len(b) {
		panic("put is broken")
	}
	sm.tx.WIndex = uint32(m)
	return
}

func (sm *Shmx) Read(b []byte) (n int, err error) {
	if !sm.ready {
		return 0, fmt.Errorf("Not Initilized")
	}

	sm.refreshRxCB()

	if sm.rx.WPktWrote == sm.rx.RPktRead {
		return 0, nil
	}

	length := sm.getUint32()
	tag := sm.getUint32()
	rd := sm.getUint32()

	if len(b) < int(length) {
		return 0, fmt.Errorf("len(b) %d < length %d", len(b), length)
	}

	if tag != 0 {
		panic("tag not zero")
	}

	if rd != 0 {
		panic("rd not zero")
	}

	// Do we need to wrap?
	var m int
	if sm.rx.RIndex+length > sm.rxRIndexWrap {
		m = copy(b, sm.m[sm.rxOffsetBase+sm.rx.RIndex:sm.rxOffsetWrap])
		sm.rx.RIndex = 0
	}

	i := sm.rxOffsetBase + sm.rx.RIndex
	j := i + length - uint32(m)
	n = copy(b[m:], sm.m[i:j])
	if m+n != int(length) {
		panic(fmt.Errorf("m %d + n %d != length %d", m, n, length))
	}

	sm.rx.RIndex += uint32(round32(n))
	if sm.rx.RIndex >= sm.rxRIndexWrap {
		sm.rx.RIndex = 0
	}
	sm.rx.RPktRead++
	sm.putRxCB()

	n += m

	return n, nil
}

func (sm *Shmx) getUint32() (u uint32) {
	i := int(sm.rxOffsetBase + sm.rx.RIndex)
	u = binary.LittleEndian.Uint32(sm.m[i:])
	sm.rx.RIndex += 4
	if sm.rx.RIndex == sm.rxRIndexWrap {
		sm.rx.RIndex = 0
	}
	return u
}

func (sm *Shmx) getConstSize() {
	sm.rx.ConstSize = binary.LittleEndian.Uint32(sm.m[sm.rxCbOffset+offsetConstS:])
	sm.tx.ConstSize = binary.LittleEndian.Uint32(sm.m[sm.txCbOffset+offsetConstS:])
}

func (sm *Shmx) refreshRxCB() {
	sm.rx.WIndex = binary.LittleEndian.Uint32(sm.m[sm.rxCbOffset+offsetWIndex:])
	sm.rx.WPktWrote = binary.LittleEndian.Uint32(sm.m[sm.rxCbOffset+offsetWPktWr:])
	sm.rx.WPktLost = binary.LittleEndian.Uint32(sm.m[sm.rxCbOffset+offsetWPktLo:])
}

func (sm *Shmx) putRxCB() {
	binary.LittleEndian.PutUint32(sm.m[sm.rxCbOffset+offsetRIndex:], sm.rx.RIndex)
	binary.LittleEndian.PutUint32(sm.m[sm.rxCbOffset+offsetRPktRe:], sm.rx.RPktRead)
}

func (sm *Shmx) refreshTxCB() {
	sm.tx.RIndex = binary.LittleEndian.Uint32(sm.m[sm.txCbOffset+offsetRIndex:])
	sm.tx.RPktRead = binary.LittleEndian.Uint32(sm.m[sm.txCbOffset+offsetRPktRe:])
}

func (sm *Shmx) putTxCB() {
	binary.LittleEndian.PutUint32(sm.m[sm.txCbOffset+offsetWIndex:], sm.tx.WIndex)
	binary.LittleEndian.PutUint32(sm.m[sm.txCbOffset+offsetWPktWr:], sm.tx.WPktWrote)
	binary.LittleEndian.PutUint32(sm.m[sm.txCbOffset+offsetWPktLo:], sm.tx.WPktLost)
}
