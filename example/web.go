// main.go
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"text/template"

	socketio "github.com/googollee/go-socket.io"
	"github.com/tomatome/grdp/glog"
	"github.com/tomatome/grdp/protocol/pdu"
)

func showPreview(w http.ResponseWriter, r *http.Request) {
	t, err := template.ParseFiles("static/html/index.html")
	if err != nil {
		w.Write([]byte(err.Error() + "\n"))
		return
	}
	w.Header().Add("Content-Type", "text/html")
	t.Execute(w, nil)

}

var (
	clients   = make([]*RdpClient, 0)
	clientsMu sync.Mutex
	activeId  string
)

func socketIO() {
	server, _ := socketio.NewServer(nil)
	server.OnConnect("/", func(so socketio.Conn) error {
		fmt.Println("OnConnect", so.ID())
		so.Emit("rdp-connect", true)
		return nil
	})
	server.OnEvent("/", "infos", func(so socketio.Conn, data interface{}) {
		soId := so.ID()
		fmt.Println("infos", soId)
		var info Info
		v, _ := json.Marshal(data)
		json.Unmarshal(v, &info)
		fmt.Println(soId, "logon infos:", info)

		g := NewRdpClient(fmt.Sprintf("%s:%s", info.Ip, info.Port), info.Width, info.Height, glog.INFO)
		g.ID = soId
		g.info = &info

		// 移除其他实例的剪贴板监听
		closeClientsEventListener(g.ID)
		err := g.Login(soId)
		if err != nil {
			fmt.Println("Login:", err)
			so.Emit("rdp-error", "{\"code\":1,\"message\":\""+err.Error()+"\"}")
			return
		}
		activeId = soId
		so.SetContext(g)

		// 将rdp实例添加到切片中
		clientsMu.Lock()
		clients = append(clients, g)
		clientsMu.Unlock()

		g.pdu.On("error", func(e error) {
			fmt.Println("on error:", e)
			so.Emit("rdp-error", "{\"code\":1,\"message\":\""+e.Error()+"\"}")
		}).On("close", func() {
			err = errors.New("close")
			fmt.Println("on close")
		}).On("success", func() {
			fmt.Println("on success")
		}).On("ready", func() {
			fmt.Println("on ready")
		}).On("bitmap", func(rectangles []pdu.BitmapData) {
			if activeId == so.ID() {
				// glog.Info(time.Now(), "on update Bitmap:", len(rectangles))
				bs := make([]Bitmap, 0, len(rectangles))
				for _, v := range rectangles {
					IsCompress := v.IsCompress()
					data := v.BitmapDataStream
					glog.Debug("data:", data)
					if IsCompress {
						//data = decompress(&v)
						//IsCompress = false
					}

					glog.Debug(IsCompress, v.BitsPerPixel)
					b := Bitmap{int(v.DestLeft), int(v.DestTop), int(v.DestRight), int(v.DestBottom),
						int(v.Width), int(v.Height), int(v.BitsPerPixel), IsCompress, data}
					// so.Emit("rdp-bitmap", []Bitmap{b})
					bs = append(bs, b)
				}
				so.Emit("rdp-bitmap", bs)
			}
		})
	})

	server.OnEvent("/", "mouse", func(so socketio.Conn, x, y uint16, button int, isPressed bool) {
		// glog.Info("mouse", x, ":", y, ":", button, ":", isPressed)
		if isPressed {
			id := so.ID()
			if activeId != id {
				activeId = so.ID()
				// 移除其他实例的剪贴板监听
				closeClientsEventListener(activeId)
				openClientsEventListener(activeId)
			}
		}
		p := &pdu.PointerEvent{}
		if isPressed {
			p.PointerFlags |= pdu.PTRFLAGS_DOWN
		}

		switch button {
		case 1:
			p.PointerFlags |= pdu.PTRFLAGS_BUTTON1
		case 2:
			p.PointerFlags |= pdu.PTRFLAGS_BUTTON2
		case 3:
			p.PointerFlags |= pdu.PTRFLAGS_BUTTON3
		default:
			p.PointerFlags |= pdu.PTRFLAGS_MOVE
		}

		p.XPos = x
		p.YPos = y
		g := so.Context().(*RdpClient)
		g.pdu.SendInputEvents(pdu.INPUT_EVENT_MOUSE, []pdu.InputEventsInterface{p})
	})

	//keyboard
	server.OnEvent("/", "scancode", func(so socketio.Conn, button uint16, isPressed bool) {
		glog.Info("scancode:", "button:", button, "isPressed:", isPressed)
		if activeId == so.ID() {
			p := &pdu.ScancodeKeyEvent{}
			p.KeyCode = button
			if !isPressed {
				p.KeyboardFlags |= pdu.KBDFLAGS_RELEASE
			}
			g := so.Context().(*RdpClient)
			g.pdu.SendInputEvents(pdu.INPUT_EVENT_SCANCODE, []pdu.InputEventsInterface{p})
		}

	})

	//wheel
	server.OnEvent("/", "wheel", func(so socketio.Conn, x, y, step uint16, isNegative, isHorizontal bool) {
		glog.Info("Received wheel event", x, ":", y, ":", step, ":", isNegative, ":", isHorizontal)
		id := so.ID()
		if activeId != id {
			activeId = so.ID()
		}
		var p = &pdu.PointerEvent{}

		if isHorizontal {
			p.PointerFlags |= pdu.PTRFLAGS_HWHEEL
		} else {
			p.PointerFlags |= pdu.PTRFLAGS_WHEEL
		}

		if isNegative {
			p.PointerFlags |= pdu.PTRFLAGS_WHEEL_NEGATIVE
		}

		p.PointerFlags |= (step & pdu.WheelRotationMask)
		p.XPos = x
		p.YPos = y

		// 打印 PointerEvent 内容
		glog.Info("PointerFlags set to:", p.PointerFlags)
		g := so.Context().(*RdpClient)
		if g == nil {
			glog.Error("Context is nil")
			return
		}

		// 发送事件并检查返回值
		g.pdu.SendInputEvents(pdu.INPUT_EVENT_MOUSE, []pdu.InputEventsInterface{p})
		// g.pdu.SendInputEvents(pdu.INPUT_EVENT_SCANCODE, []pdu.InputEventsInterface{p})
	})

	server.OnError("/", func(so socketio.Conn, err error) {
		defer func() {
			if r := recover(); r != nil {
				fmt.Println("Recovered from panic:", r)
			}
		}()

		if so == nil {
			fmt.Println("Connection is nil")
			return
		}
		ctx := so.Context()
		if ctx == nil {
			fmt.Println("Context is nil")
			return
		}
		soId := so.ID()
		// 处理错误
		fmt.Println("Error occurred:", err)
		fmt.Println("error:", err)
		so.Emit("rdp-error", err)
		g := so.Context().(*RdpClient)
		if g != nil {
			// g.x224.Close()
			g.channels.Close()
			g.mcs.Close()
			g.sec.Close()
			// g.pdu.Close()
			g.tpkt.Close()
		}
		so.Close()
		deleteClient(soId)
	})

	server.OnDisconnect("/", func(so socketio.Conn, s string) {
		defer func() {
			if r := recover(); r != nil {
				fmt.Println("Recovered from panic:", r)
			}
		}()
		if so == nil || so.Context() == nil {
			so.Close()
			fmt.Println("OnDisconnect socket close")
			return
		}
		fmt.Println("OnDisconnect:", s)
		so.Emit("rdp-error", "{code:1,message:"+s+"}")

		g := so.Context().(*RdpClient)
		if g != nil {
			// 移除监听
			g.channels.Close()
			g.mcs.Close()
			g.sec.Close()
			// g.pdu.Close()
			g.channels.Close()
			g.tpkt.Close()
		}
		so.Close()
		deleteClient(so.ID())
	})
	go server.Serve()
	// defer server.Close()

	http.Handle("/socket.io/", server)

	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	http.Handle("/css/", http.FileServer(http.Dir("static")))
	http.Handle("/js/", http.FileServer(http.Dir("static")))
	http.Handle("/img/", http.FileServer(http.Dir("static")))
	http.HandleFunc("/", showPreview)

	log.Println("Serving at localhost:8088...")
	log.Fatal(http.ListenAndServe(":8088", nil))
}

// 移除其他实例的clipboard监听
func closeClientsEventListener(id string) {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	for _, client := range clients {
		if client.ID != id {
			client.channels.RemoveListen()
		}
	}
}

// 打开实例的clipboard监听
func openClientsEventListener(id string) {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	for _, client := range clients {
		if client.ID == id {
			client.channels.RestartListen()
		}
	}
}

// 删除实例
func deleteClient(id string) {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	for i, client := range clients {
		if client.ID == id {
			clients = append(clients[:i], clients[i+1:]...)
			break
		}
	}
}
