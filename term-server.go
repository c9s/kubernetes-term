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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/client-go/tools/remotecommand"

	scheme "k8s.io/client-go/kubernetes/scheme"

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

type TermConnectPayload struct {
	Namespace     string `json:"namespace"`
	PodName       string `json:"pod"`
	ContainerName string `json:"container"`
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
	log.Printf("emit %s -> '%v'", w.Event, p)
	return len(p), err
}

func NewExecRequest(clientset *kubernetes.Clientset, p TermConnectPayload) *rest.Request {
	rest := clientset.RESTClient()
	req := rest.Post().
		Prefix("/api/v1").
		Resource("pods").
		Name(p.PodName).
		Namespace("default").
		SubResource("exec").
		Param("container", p.ContainerName).
		Param("stdin", "true").
		Param("stdout", "true").
		Param("stderr", "true").
		Param("tty", "true").
		Param("command", "/bin/sh")

	req.VersionedParams(&corev1.PodExecOptions{
		Container: p.ContainerName,
		Command:   []string{"sh"},

		// turn on the stdin if we have the input device connected
		Stdin: true,

		// read the stdout
		Stdout: true,

		// read the stderr
		Stderr: true,

		// tty is not allocated for the call
		TTY: false,
	}, scheme.ParameterCodec)
	return req
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
		log.Printf("client connected: id=%s", socket.Id())

		// emit an "open" message to the client
		socket.Emit("open")

		var rawTerm = true

		var stdoutWriter = SocketIoWriter{
			Event:  "term:stdout",
			Socket: socket,
		}

		var stderrWriter = SocketIoWriter{
			Event:  "term:stderr",
			Socket: socket,
		}

		var stdinReader = SocketIoReader{
			Event:  "term:stdin",
			Socket: socket,
			Buffer: make(chan []byte, 30),
		}

		var sizeQueue = &SocketIoSizeQueue{
			C: make(chan *remotecommand.TerminalSize, 10),
		}

		socket.On("term:connect", func(data string) {
			p := TermConnectPayload{}
			if err := json.Unmarshal([]byte(data), &p); err != nil {
				log.Println("json decode failed:", err)
				return
			}

			if len(p.Namespace) == 0 {
				p.Namespace = "default"
			}

			if len(p.PodName) == 0 {
				log.Println("pod name must be specified")
				return
			}

			pod, err := clientset.Core().Pods(p.Namespace).Get(p.PodName, metav1.GetOptions{})
			if err != nil {
				log.Printf("pod %s not found.", p.PodName)
				return
			}
			_ = pod

			// run the goroutine
			fn := func() error {
				req := NewExecRequest(clientset, p)

				log.Println("Created request:", req.URL())

				exec, err := remotecommand.NewSPDYExecutor(restConfig, http.MethodPost, req.URL())
				if err != nil {
					log.Print("failed to create spdy executor")
					return err
				}

				socket.Emit("term:connected")

				// start streaming
				return exec.Stream(remotecommand.StreamOptions{
					Stdin:             &stdinReader,
					Stdout:            &stdoutWriter,
					Stderr:            &stderrWriter,
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
		})

		socket.On("term:stdin", func(data string) {
			stdinReader.Write([]byte(data))
		})

		socket.On("term:resize", func(data string) {
			log.Printf("term:resize %s", data)

			p := TermSizePayload{}
			err := json.Unmarshal([]byte(data), &p)
			if err != nil {
				log.Println("term:resize error:", err)
				return
			}
			sizeQueue.Push(p.Columns, p.Rows)
		})

		socket.On("disconnection", func(data string) {
			log.Printf("client disconnected. id=%s", socket.Id())
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
