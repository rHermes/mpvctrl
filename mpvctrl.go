package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"sync"

	"github.com/Microsoft/go-winio"
	"github.com/buger/jsonparser"
	"github.com/pressly/chi"
)

// TODO(rhermes): List the content of \\.\pipe\ and filter over some pattern,
// to get the current mpv instances, and present them as choices in the root
// form. This will allow me to have multiple mpv instance open and also allow me
// to set it up in my mpv conf, so that a named pipe is open automatically.

// These don't have the the last few bytes, as we append a request_id.
var (
	JPC_PAUSE_ON      = []byte(`{"command": ["set_property", "pause", true]`)
	JPC_PAUSE_OFF     = []byte(`{"command": ["set_property", "pause", false]`)
	JPC_PAUSE_TOGGLE_ = []byte(`{"command": ["cycle", "pause"]`)

	JPC_OSC_OFF = []byte(`{"command": ["script-message", "osc-visibility", "never"]`)
	JPC_OSC_ON  = []byte(`{"command": ["script-message", "osc-visibility", "always"]`)

	JPC_PLAYLIST_PREV = []byte(`{"command": ["playlist-prev"]`)
	JPC_PLAYLIST_NEXT = []byte(`{"command": ["playlist-next"]`)

	JPC_CHAPTER_PREV = []byte(`{"command": ["add", "chapter", -1]`)
	JPC_CHAPTER_NEXT = []byte(`{"command": ["add", "chapter", 1]`)

	JPC_PRESS_LEFT  = []byte(`{"command": ["keypress", "LEFT"]`)
	JPC_PRESS_RIGHT = []byte(`{"command": ["keypress", "RIGHT"]`)
)

type MPVClient struct {
	nc net.Conn
	rw *bufio.ReadWriter
	wg sync.WaitGroup
	rd *rand.Rand

	// This is used for routing
	i2c    map[uint32](chan []byte)
	i2cMtx sync.Mutex
}

func (mc *MPVClient) Close() error {
	// If we currently have outstanding return values for commands, we wait.
	mc.wg.Wait()
	return mc.nc.Close()
}

func (mc *MPVClient) inputMonitor() {
	for {
		dbt, err := mc.rw.ReadBytes('\n')
		if err != nil {
			if err != io.EOF {
				log.Println(err.Error())
			}
			break
		}

		// We need to check if this is an event or not.
		ename, err := jsonparser.GetString(dbt, "event")
		if err != nil && err != jsonparser.KeyPathNotFoundError {
			log.Fatal(err)
		}

		// Is this a command event?
		if err == jsonparser.KeyPathNotFoundError {
			mc.i2cMtx.Lock()

			// Get msg id.
			msgID, err := jsonparser.GetInt(dbt, "request_id")
			if err != nil {
				log.Fatal(err)
			}

			ch, ok := mc.i2c[uint32(msgID)]
			if !ok {
				log.Fatal("We haven't seen this ID before!")
			}
			go func(msg []byte) {
				ch <- msg
				close(ch)
			}(dbt)

			mc.i2cMtx.Unlock()
			mc.wg.Done()

		} else {
			log.Printf("We got event ( %s ): %s", ename, string(dbt))
		}
	}
}

// Helper function to avoid code repetition.
func (mc *MPVClient) sendCommand(cmd []byte) (<-chan []byte, error) {
	// BUG(rhermes): There could be a problem here if the error happens,
	// but if I Put the wg.Add after any of these, there could be race conditions.

	mc.i2cMtx.Lock()
	defer mc.i2cMtx.Unlock()

	msgID := mc.rd.Uint32()
	ncmd := []byte(fmt.Sprintf("%s, \"request_id\": %d}\n", cmd, msgID))
	if _, err := mc.rw.Write(ncmd); err != nil {
		return nil, err
	}
	if err := mc.rw.Flush(); err != nil {
		return nil, err
	}

	// Make the one off channel
	mc.i2c[msgID] = make(chan []byte)
	mc.wg.Add(1)

	return mc.i2c[msgID], nil
}

// Export the commands we need.
func (mc *MPVClient) PauseToggle() (<-chan []byte, error) { return mc.sendCommand(JPC_PAUSE_TOGGLE_) }

func (mc *MPVClient) OSCOff() (<-chan []byte, error) { return mc.sendCommand(JPC_OSC_OFF) }
func (mc *MPVClient) OSCOn() (<-chan []byte, error)  { return mc.sendCommand(JPC_OSC_ON) }

func (mc *MPVClient) PlaylistPrev() (<-chan []byte, error) { return mc.sendCommand(JPC_PLAYLIST_PREV) }
func (mc *MPVClient) PlaylistNext() (<-chan []byte, error) { return mc.sendCommand(JPC_PLAYLIST_NEXT) }

func (mc *MPVClient) ChapterPrev() (<-chan []byte, error) { return mc.sendCommand(JPC_CHAPTER_PREV) }
func (mc *MPVClient) ChapterNext() (<-chan []byte, error) { return mc.sendCommand(JPC_CHAPTER_NEXT) }

func (mc *MPVClient) PressLeft() (<-chan []byte, error)  { return mc.sendCommand(JPC_PRESS_LEFT) }
func (mc *MPVClient) PressRight() (<-chan []byte, error) { return mc.sendCommand(JPC_PRESS_RIGHT) }

func NewMPVClient(pipeName string) (*MPVClient, error) {
	var mc MPVClient

	nc, err := winio.DialPipe(pipeName, nil)
	if err != nil {
		return nil, err
	}
	mc.nc = nc
	mc.rw = bufio.NewReadWriter(bufio.NewReader(nc), bufio.NewWriter(nc))
	mc.rd = rand.New(rand.NewSource(0))
	mc.i2c = make(map[uint32](chan []byte))

	go mc.inputMonitor()

	return &mc, nil
}

func basicHandler(f func() (<-chan []byte, error)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		res, err := f()
		if err != nil {
			log.Println(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Println(string(<-res))
		http.Redirect(w, r, "/", http.StatusFound)
	}
}

func main() {
	// Setup logger
	log.SetFlags(log.Flags() | log.Llongfile)

	mc, err := NewMPVClient(`\\.\pipe\mpv_socket`)
	if err != nil {
		log.Fatal(err)
	}
	defer mc.Close()

	r := chi.NewRouter()

	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `
		<html>
		<head>
			<meta charset="utf-8">
			<meta http-equiv="x-ua-compatible" content="ie=edge">
			<meta name="viewport" content="width=device-width, initial-scale=1">
		</head>
		<body>
			<h1>Controls</h1>
			<ul>
				<li><a href="/api/pauseToggle">pauseToggle</a></li>
				
				<li></li>
				
				<li><a href="/api/oscOff">oscOff</a></li>
				<li><a href="/api/oscOn">oscOn</a></li>
				
				<li></li>
				
				<li><a href="/api/playlistPrev">playlistPrev</a></li>
				<li><a href="/api/playlistNext">playlistNext</a></li>
				<li></li>
				
				<li><a href="/api/chapterPrev">chapterPrev</a></li>
				<li><a href="/api/chapterNext">chapterNext</a></li>
				
				<li></li>
				
				<li><a href="/api/pressLeft">pressLeft</a></li>
				<li><a href="/api/pressRight">pressRight</a></li>
			</ul>
		</body>
		</html>
		`)
	})

	// Pause
	r.Get("/api/pauseToggle", basicHandler(mc.PauseToggle))

	// OSC
	r.Get("/api/oscOff", basicHandler(mc.OSCOff))
	r.Get("/api/oscOn", basicHandler(mc.OSCOn))

	// Playlist
	r.Get("/api/playlistNext", basicHandler(mc.PlaylistNext))
	r.Get("/api/playlistPrev", basicHandler(mc.PlaylistPrev))

	// Playlist
	r.Get("/api/chapterNext", basicHandler(mc.ChapterNext))
	r.Get("/api/chapterPrev", basicHandler(mc.ChapterPrev))

	// Keys
	r.Get("/api/pressLeft", basicHandler(mc.PressLeft))
	r.Get("/api/pressRight", basicHandler(mc.PressRight))

	http.ListenAndServe("192.168.1.177:3333", r)
}
