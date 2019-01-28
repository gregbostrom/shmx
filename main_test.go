package shmx_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"shmx"
	"testing"
)

func TestHandler(t *testing.T) {

	var mStats shmx.Stats
	var sStats shmx.Stats

	path := "shmx.nic"
	os.Remove(path)

	master := new(shmx.Shmx)
	err := master.Attach(shmx.Master, path)
	if err != nil {
		t.Errorf(fmt.Sprintf("master.Attach failed %v", err))
	}

	slave := new(shmx.Shmx)
	err = slave.Attach(shmx.Slave, path)
	if err != nil {
		t.Errorf(fmt.Sprintf("slave.Attach failed %v", err))
	}

	sdata := "0123456789ABCDEF!@#$%^&*()_+=-{}|][:;?/><,.~"

	for {
		if 2*len(sdata) > shmx.ShmxMaxLen {
			break
		}
		sdata += sdata

		echoTest(t, master, slave, sdata)
	}

	for i := 0; i < 10000; i++ {
		echoTest(t, master, slave, sdata)
	}

	sendUntilDrop(t, slave, sdata)
	readUntilEmpty(t, master)

	master.Stats(&mStats)
	slave.Stats(&sStats)

	master.Detach()
	slave.Detach()

	fmt.Println("Slave RPktRead", sStats.RPktRead)
	fmt.Println("Slave WPktWrote", sStats.WPktWrote)
	fmt.Println("Slave WPktLost", sStats.WPktLost)

	fmt.Println("master RPktRead", mStats.RPktRead)
	fmt.Println("master WPktWrote", mStats.WPktWrote)
	fmt.Println("master WPktLost", mStats.WPktLost)
}

func echoTest(t *testing.T, master *shmx.Shmx, slave *shmx.Shmx, s string) {
	var err error
	var n int

	b := bytes.NewBufferString(s)
	_, err = b.WriteTo(slave)
	if err != nil {
		t.Errorf(fmt.Sprintf("WriteTo(slave) failed %v", err))
	}

	p := make([]byte, shmx.ShmxMaxLen)

	n, err = master.Read(p)
	if err != nil {
		t.Errorf(fmt.Sprintf("master.Read failed %v", err))
	}

	n, err = master.Write(p[0:n])
	if err != nil {
		t.Errorf(fmt.Sprintf("master.Write failed %v", err))
	}

	n, err = slave.Read(p)
	if err != nil {
		t.Errorf(fmt.Sprintf("slave.Read failed %v", err))
	}

	s2 := string(p[0:n])
	if s != s2 {
		fmt.Println(s)
		t.Errorf("echo failed")
	}
}

func sendUntilDrop(t *testing.T, sm *shmx.Shmx, s string) {
	var b bytes.Buffer
	var err error
	var i int

	n := len(s)
	n64 := int64(n)

	for i = 0; i < 1000000; i++ {
		var m int
		m, err = b.WriteString(s)
		if (m != n) || (err != nil) {
			t.Errorf("bytes.Buffer WriteString")
		}
		n64, err = b.WriteTo(sm)
		if (n64 != int64(n)) || (err != nil) {
			fmt.Println("wrote", n64, "!=", n, ":", err)
			break
		}
	}

	fmt.Println("sendUntilDrop:", i, "of size", n)

	if err != nil {
		if err == io.ErrShortWrite {
			// expected for this test
		} else {
			t.Errorf(fmt.Sprintf("WriteTo(slave) failed %v", err))
		}
	}
}

func readUntilEmpty(t *testing.T, sm *shmx.Shmx) {
	var err error
	var i int
	var n int

	p := make([]byte, shmx.ShmxMaxLen)

	for i = 0; ; i++ {
		n, err = sm.Read(p)
		if n == 0 || err != nil {
			break
		}
	}
	if err != nil {
		t.Errorf(fmt.Sprintf("master.Read failed %v", err))
	}

	fmt.Println("readUntilEmpty", i)
}
