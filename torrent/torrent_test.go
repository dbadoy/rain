package torrent_test

import (
	"encoding/hex"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/cenkalti/log"
	"github.com/cenkalti/rain/internal/logger"
	"github.com/cenkalti/rain/torrent"
	"github.com/cenkalti/rain/torrent/metainfo"
	"github.com/cenkalti/rain/torrent/storage/filestorage"
)

var (
	torrentFile           = filepath.Join("testdata", "10mb.torrent")
	torrentInfoHashString = "0a8e2e8c9371a91e9047ed189ceffbc460803262"
	torrentDataDir        = "testdata"
	torrentName           = "10mb"
	timeout               = 10 * time.Second
)

func init() {
	logger.SetLevel(log.DEBUG)
}

func newFileStorage(t *testing.T, dir string) *filestorage.FileStorage {
	sto, err := filestorage.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return sto
}

func TestDownloadMagnet(t *testing.T) {
	where, err := ioutil.TempDir("", "rain-")
	if err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(torrentFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	mi, err := metainfo.New(f)
	if err != nil {
		t.Fatal(err)
	}
	opt1 := torrent.Options{
		Info: mi.Info.Bytes,
	}
	t1, err := opt1.NewTorrent(mi.Info.Hash[:], newFileStorage(t, torrentDataDir))
	if err != nil {
		t.Fatal(err)
	}
	defer t1.Close()

	opt2 := torrent.Options{}
	ih, err := hex.DecodeString(torrentInfoHashString)
	if err != nil {
		t.Fatal(err)
	}
	t2, err := opt2.NewTorrent(ih, newFileStorage(t, where))
	if err != nil {
		t.Fatal(err)
	}
	defer t2.Close()

	t1.Start()
	t2.Start()

	var port int
	select {
	case port = <-t1.NotifyListen():
	case err = <-t1.NotifyError():
		t.Fatal(err)
	case <-time.After(timeout):
		panic("seeder is not ready")
	}

	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
	t2.AddPeers([]*net.TCPAddr{addr})

	select {
	case <-t2.NotifyComplete():
	case err = <-t2.NotifyError():
		t.Fatal(err)
	case <-time.After(timeout):
		panic("download did not finish")
	}

	cmd := exec.Command("diff", "-rq",
		filepath.Join(torrentDataDir, torrentName),
		filepath.Join(where, torrentName))
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	err = os.RemoveAll(where)
	if err != nil {
		t.Fatal(err)
	}
}
