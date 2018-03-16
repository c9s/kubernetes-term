package main

import (
	// "bytes"
	"flag"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	// "sync"

	"encoding/json"

	"github.com/rs/cors"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"

	api "k8s.io/api/core/v1"

	// "k8s.io/apimachinery/pkg/runtime"
	// "k8s.io/apimachinery/pkg/runtime/schema"

	// "k8s.io/client-go/pkg/api"
	"k8s.io/client-go/tools/remotecommand"

	scheme "k8s.io/client-go/kubernetes/scheme"

	// "k8s.io/kubernetes/pkg/util/interrupt"

	// "k8s.io/apimachinery/pkg/runtime"

	// socket.io
	socketio "github.com/c9s/go-socket.io"
)

type Payload struct {
	Data string `json:"data"`
}

func HomeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // for windows
}

func FindConfig() (string, bool) {
	if home := HomeDir(); home != "" {
		p := filepath.Join(home, ".kube", "config")
		_, err := os.Stat(p)
		if err != nil {
			return "", false
		}
		return p, true
	}
	return "", false
}

func Load(context string, kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		_, err := os.Stat(kubeconfig)
		if err == nil {
			return clientcmd.BuildConfigFromFlags(context, kubeconfig)
		}
	}
	mykubeconfig, found := FindConfig()
	if found {
		log.Println("found kubeconfig:", mykubeconfig)
		return clientcmd.BuildConfigFromFlags(context, mykubeconfig)
	}
	return rest.InClusterConfig()
}

func LoadConfig() (*rest.Config, error) {
	return Load("gke_linker-aurora_asia-east1-a_aurora-prod", "")
	// return Load("gke_linker-aurora_asia-east1-a_aurora-jenkins", "")
}

type TermSizePayload struct {
	Columns uint16 `json:"cols"`
	Rows    uint16 `json:"rows"`
}

type SocketIoSizeQueue struct {
	C chan *remotecommand.TerminalSize
}

func (q *SocketIoSizeQueue) Push(cols uint16, rows uint16) {
	q.C <- &remotecommand.TerminalSize{Width: cols, Height: rows}
}

func (q *SocketIoSizeQueue) Next() *remotecommand.TerminalSize {
	return <-q.C
}

type SocketIoReader struct {
	Event  string
	Socket socketio.Socket
	Buffer chan []byte
}

func (r *SocketIoReader) Write(p []byte) (n int, err error) {
	log.Println(r.Event, p)
	r.Buffer <- p
	return len(p), nil
}

func (r *SocketIoReader) Read(p []byte) (n int, err error) {
	data := <-r.Buffer
	n = copy(p, data)
	return n, err
}

type SocketIoWriter struct {
	Event  string
	Socket socketio.Socket
}

func (w *SocketIoWriter) Write(p []byte) (n int, err error) {
	data := string(p)
	if err := w.Socket.Emit(w.Event, data); err != nil {
		log.Println("emit error:", err)
	}
	log.Printf("emit %s -> '%s'", w.Event, data)
	return len(p), err
}

func main() {
	flag.Parse()
	restConfig, err := LoadConfig()
	if err != nil {
		log.Fatal(err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		log.Fatal(err)
	}

	io, err := socketio.NewServer(nil)
	if err != nil {
		panic(err)
	}

	// Get topic from client, and add client to topic
	io.On("connection", func(socket socketio.Socket) {
		log.Printf("Client connected: id=%s", socket.Id())

		// emit an "open" message to the client
		socket.Emit("open")

		stdoutHandler := SocketIoWriter{
			Event:  "term:stdout",
			Socket: socket,
		}
		stderrHandler := SocketIoWriter{
			Event:  "term:stderr",
			Socket: socket,
		}
		stdinHandler := SocketIoReader{
			Event:  "term:stdin",
			Socket: socket,
			Buffer: make(chan []byte, 30),
		}

		podName := "mongo-0"
		containerName := "mongo"
		rawTerm := true

		var sizeQueue = &SocketIoSizeQueue{
			C: make(chan *remotecommand.TerminalSize, 10),
		}

		// run the goroutine
		fn := func() error {
			rest := clientset.RESTClient()
			req := rest.Post().
				Prefix("/api/v1").
				Resource("pods").
				Name(podName).
				Namespace("default").
				SubResource("exec").
				Param("container", containerName).
				Param("stdin", "true").
				Param("stdout", "true").
				Param("stderr", "true").
				Param("tty", "true").
				Param("command", "/bin/sh")

			req.VersionedParams(&api.PodExecOptions{
				Container: containerName,
				Command:   []string{"sh"},

				// turn on the stdin if we have the input device connected
				Stdin: false,

				// read the stdout
				Stdout: true,

				// read the stderr
				Stderr: true,

				TTY: true,
			}, scheme.ParameterCodec)

			log.Println(req.URL())
			log.Printf("%v\n", req)

			exec, err := remotecommand.NewSPDYExecutor(restConfig, http.MethodPost, req.URL())
			if err != nil {
				log.Print("failed to create spdy executor")
				return err
			}

			return exec.Stream(remotecommand.StreamOptions{
				Stdin:             &stdinHandler,
				Stdout:            &stdoutHandler,
				Stderr:            &stderrHandler,
				Tty:               rawTerm,
				TerminalSizeQueue: sizeQueue,
			})
		}

		go func() {
			log.Println("Sending exec request ...")
			if err := fn(); err != nil {
				log.Fatal("container exec connection failed:", err)
			}
			log.Println("exec connection terminated.")

			socket.Emit("term:terminated")
		}()

		socket.On("term:resize", func(data string) {
			payload := TermSizePayload{}
			err := json.Unmarshal([]byte(data), &payload)
			if err != nil {
				log.Println("term:resize error:", err)
				return
			}
			sizeQueue.Push(payload.Columns, payload.Rows)
		})

		socket.On("term:stdin", func(data string) {
			stdinHandler.Write([]byte(data))
		})

		socket.On("disconnection", func(data string) {
			log.Printf("Client disconnected. id=%s", socket.Id())
		})
	})

	io.On("error", func(client socketio.Socket, err error) {
		log.Printf("socketio: error: id=%s err=%s", client.Id(), err)
	})

	corsAllowAll := cors.New(cors.Options{
		// This is for webpack-dev-server and socket.io
		// AllowedOrigins:   []string{"https://localhost:8080"},
		AllowedOrigins:   []string{"*"},
		AllowCredentials: true,
		Debug:            false,
	})

	fs := http.FileServer(http.Dir("./node_modules"))
	http.Handle("/node_modules/", http.StripPrefix("/node_modules/", fs))
	http.Handle("/socket.io/", corsAllowAll.Handler(io))
	http.HandleFunc("/", serveTemplate)

	log.Println("Listening...")
	http.ListenAndServe(":3000", nil)
}

func serveTemplate(w http.ResponseWriter, r *http.Request) {
	bytes, _ := ioutil.ReadFile("index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf8")
	w.Write(bytes)
}
