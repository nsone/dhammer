package hammer

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"dhammer/config"
	"dhammer/generator"
	"dhammer/handler"
	"dhammer/socketeer"
	"dhammer/stats"

	"github.com/corneldamian/httpway"
	"github.com/gorilla/handlers"
	"github.com/julienschmidt/httprouter"
)

/*
	TODO:
		Option structs should stop being references.
*/

type Hammer struct {
	options          config.HammerConfig
	socketeerOptions *config.SocketeerOptions
	logChannel       chan string
	statsChannel     chan string
	errorChannel     chan error

	handler   handler.Handler
	generator generator.Generator
	stats     stats.Stats
	socketeer *socketeer.RawSocketeer

	apiServer *httpway.Server
}

func New(s *config.SocketeerOptions, o config.HammerConfig) *Hammer {

	h := Hammer{
		socketeerOptions: s,
		options:          o,
		logChannel:       make(chan string, 1000),
		statsChannel:     make(chan string, 1000),
		errorChannel:     make(chan error, 1000),
	}

	return &h
}

func (h *Hammer) Init(apiAddr string, apiPort int) error {

	var err error

	log.SetFlags(log.LstdFlags | log.LUTC)

	if h.stats, err = stats.New(h.options, h.addStats, h.addError); err != nil {
		return err
	}

	if err = h.stats.Init(); err != nil {
		return err
	}

	h.socketeer = socketeer.NewRawSocketeer(h.socketeerOptions, h.addLog, h.addError)
	if err = h.socketeer.Init(); err != nil {
		return err
	}

	if h.handler, err = handler.New(h.socketeer, h.options, h.addLog, h.addError, h.stats.AddStat); err != nil {
		return err
	}

	if err := h.handler.Init(); err != nil {
		return err
	}

	h.socketeer.SetReceiver(h.handler.ReceiveMessage)

	if h.generator, err = generator.New(h.socketeer, h.options, h.addLog, h.addError, h.stats.AddStat); err != nil {
		return err
	}

	if err = h.generator.Init(); err != nil {
		return err
	}

	h.initApiServer(apiAddr, apiPort)

	return nil
}

func (h *Hammer) deInit() {
	var err error

	if err = h.socketeer.DeInit(); err != nil {
		h.addError(err)
	}

	if err = h.handler.DeInit(); err != nil {
		h.addError(err)
	}

	if err = h.generator.DeInit(); err != nil {
		h.addError(err)
	}

	if err = h.stats.DeInit(); err != nil {
		h.addError(err)
	}
}
func (h *Hammer) Run() error {

	var wg sync.WaitGroup

	log.Print("INFO: Starting error channel reader.")
	wg.Add(1)
	go func() {
		var err error

		for err = range h.errorChannel {
			log.Print("ERROR: " + err.Error())
		}
		wg.Done()
		log.Print("INFO: Stopped error channel reader.")
	}()

	log.Print("INFO: Starting stats.")
	wg.Add(1)
	go func() {
		h.stats.Run()
		wg.Done()
		log.Print("INFO: Stopped stats.")
	}()

	log.Print("INFO: Starting writer.")
	wg.Add(1)
	go func() {
		h.socketeer.RunWriter()
		wg.Done()
		log.Print("INFO: Stopped writer.")
	}()

	log.Print("INFO: Starting handler.")
	wg.Add(1)
	go func() {
		h.handler.Run()
		wg.Done()
		log.Print("INFO: Stopped handler.")
	}()

	log.Print("INFO: Starting listener.")
	wg.Add(1)
	go func() {
		h.socketeer.RunListener()
		wg.Done()
		log.Print("INFO: Stopped listener.")
	}()

	log.Print("INFO: Starting log channel reader.")
	wg.Add(1)
	go func() {
		var msg string

		for msg = range h.logChannel {
			log.Print("INFO: " + msg)
		}
		wg.Done()
		log.Print("INFO: Stopped log channel reader.")
	}()

	log.Print("INFO: Starting stats channel reader.")
	wg.Add(1)
	go func() {
		var msg string

		for msg = range h.statsChannel {
			log.Print(msg)
		}
		wg.Done()
		log.Print("INFO: Stopped stats channel reader.")
	}()

	log.Print("INFO: Starting generator.")
	wg.Add(1)
	go func() {
		h.generator.Run()
		log.Print("INFO: Stopped generator.")
		log.Print("INFO: Going to stop everything else...")
		h.stop()
		wg.Done()
	}()

	log.Print("INFO: Starting API server.")
	h.startApiServer()
	log.Print("INFO: Stopped API server.")

	wg.Wait()

	return nil
}

func (h *Hammer) addError(e error) bool {
	select {
	case h.errorChannel <- e:
		return true
	default:
	}
	return false
}

func (h *Hammer) addLog(s string) bool {
	select {
	case h.logChannel <- s:
		return true
	default:
	}

	return false
}

func (h *Hammer) addStats(s string) bool {
	select {
	case h.statsChannel <- s:
		return true
	default:
	}

	return false
}

func (h *Hammer) Stop() {
	// All "stop" calls should block.
	// This will make sure no new payloads go TO the writer FROM the generator.
	if err := h.generator.Stop(); err != nil {
		panic(err)
	}
}

func (h *Hammer) stop() {
	var err error

	// All "stop" calls should block.

	if err = h.stopApiServer(); err != nil {
		h.addError(err)
	}

	if err = h.socketeer.StopListener(); err != nil { // This will make sure no new messages are sent TO the handler.
		h.addError(err)
	}

	if err = h.handler.Stop(); err != nil { // This will make sure no new payloads go TO the writer FROM the handler.
		h.addError(err)
	}

	if err = h.socketeer.StopWriter(); err != nil { // This will stop any writing to the underlying socket and stop any potential error or message logging.
		h.addError(err)
	}

	if err = h.stats.Stop(); err != nil { // This should be the last place that could send errors or logs.
		h.addError(err)
	}

	/*
	 Err doesn't get returned here because it uses h.addError directly.
	 There may eventually be multiple points in it where err != nil, but I want all the DeInit steps to complete, so I won't return err.
	 Instead, I addError so it gets reported and I keep going.
	*/
	h.deInit()

	close(h.errorChannel)
	close(h.logChannel)
	close(h.statsChannel)
}

/*************************
 * API Server
 *************************/

func (h *Hammer) statsHandler(response http.ResponseWriter, request *http.Request, ps httprouter.Params) {
	fmt.Fprintf(response, h.stats.String())
}

func (h *Hammer) updateHandler(response http.ResponseWriter, request *http.Request, ps httprouter.Params) {

	body, err := ioutil.ReadAll(request.Body)

	if err != nil {
		h.addError(err)
		http.Error(response, err.Error(), 400)
		return
	}

	var details map[string]interface{}

	err = json.Unmarshal(body, &details)

	if err != nil {
		h.addError(err)
		http.Error(response, err.Error(), 400)
		return
	}

	if err := h.generator.Update(details); err != nil {
		h.addError(err)
		http.Error(response, err.Error(), 500)
		return
	}

	fmt.Fprintf(response, "{\"status\": \"ok\"}")
}

func (h *Hammer) initApiServer(apiAddr string, apiPort int) {
	r := httprouter.New()
	r.GET("/stats",
		func(response http.ResponseWriter, request *http.Request, ps httprouter.Params) {
			h.statsHandler(response, request, ps)
		})

	r.PUT("/update",
		func(response http.ResponseWriter, request *http.Request, ps httprouter.Params) {
			h.updateHandler(response, request, ps)
		})

	h.apiServer = httpway.NewServer(nil)
	h.apiServer.Handler = handlers.LoggingHandler(os.Stdout, r)
	h.apiServer.Addr = fmt.Sprintf("%s:%d", apiAddr, apiPort)
}

func (h *Hammer) startApiServer() {
	if err := h.apiServer.Start(); err != nil {
		h.addError(err)
	}

	if err := h.apiServer.WaitStop(2 * time.Second); err != nil {
		h.addError(err)
	}
}

func (h *Hammer) stopApiServer() error {
	if err := h.apiServer.Stop(); err != nil {
		return err
	}

	return nil
}
