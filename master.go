package master

import (
	"fmt"
	"time"

	"github.com/InVisionApp/go-logger"
	"github.com/InVisionApp/go-logger/shims/logrus"
	"github.com/relistan/go-director"
	"github.com/satori/go.uuid"

	"github.com/InVisionApp/go-master/backend"
	"github.com/InVisionApp/go-master/safe"
)

type Master interface {
	Start() error
	Stop() error
	IsMaster() bool

	Status() (interface{}, error)
}

type master struct {
	// the uuid should change each time the master is started up
	uuid     string
	version  string
	isMaster *safe.SafeBool
	info     *backend.MasterInfo

	heartBeatFreq time.Duration

	startHook func()
	stopHook  func()

	heartBeat director.Looper

	// all errors occurring on async work is sent back on here
	errors chan error
	log    log.Logger

	lock backend.MasterLock
}

type MasterConfig struct {
	Version            string
	HeartBeatFrequency time.Duration

	MasterLock backend.MasterLock

	// StartHook func is called as soon as a master lock is achieved.
	// It is the callback to signal becoming a master
	StartHook func()

	// StopHook func is called when the master lock is lost.
	// It is the callback to signal that it is no longer the master.
	// It is not called when the master is stopped manually
	StopHook func()

	Err    chan error
	Logger log.Logger
}

func New(cfg *MasterConfig) Master {
	if cfg.Logger == nil {
		cfg.Logger = logrus.New(nil)
	}

	return &master{
		uuid:          generateUUID().String(), // pick a unique ID for the master
		version:       cfg.Version,
		isMaster:      safe.NewBool(),
		heartBeatFreq: cfg.HeartBeatFrequency,
		lock:          cfg.MasterLock,

		startHook: cfg.StartHook,
		stopHook:  cfg.StopHook,

		heartBeat: director.NewImmediateTimedLooper(director.FOREVER, cfg.HeartBeatFrequency, nil),

		//TODO: implement error proxy
		//TODO: check for a nil err chan
		errors: cfg.Err,
		log:    cfg.Logger,
	}
}

// validate that all necessary configuration/components are there
func (m *master) validate() error {
	//TODO: implement

	return nil
}

func (m *master) Start() error {
	// can not start if already a master
	if m.isMaster.Val() {
		return fmt.Errorf("already master since %v", m.info.StartedAt)
	}

	if err := m.validate(); err != nil {
		return fmt.Errorf("invalid master setup: %v", err)
	}

	// kick off the heartbeat loop
	go m.runHeartBeat()

	return nil
}

func (m *master) runHeartBeat() {
	m.heartBeat.Loop(func() error {
		if !m.isMaster.Val() {
			// attempt to become the master
			if m.becomeMaster() {
				// became the master
				if m.startHook != nil {
					// run the start hook in a routine so it doesn't block
					go m.startHook()
				}
			}

			// continue
			return nil
		}

		// I am the master!

		// run the heartbeat
		if err := m.lock.WriteHeartbeat(m.info); err != nil {
			m.sendError(err)
			// if heartbeat fails or master lock lost, stop the tasks
			m.cleanupMaster()
		}

		//continue
		return nil
	})
}

func (m *master) becomeMaster() bool {
	mi := &backend.MasterInfo{
		MasterID: m.uuid,
		Version:  m.version,
	}

	if err := m.lock.Lock(mi); err != nil {
		return false
	}

	m.isMaster.SetTrue()
	m.info = mi

	return true
}

// Call on this to do cleanup after a master lock is lost.
func (m *master) cleanupMaster() {
	// this is done first so that anything
	// reading this will get an accurate value
	m.isMaster.SetFalse()
	m.info = &backend.MasterInfo{}

	if m.stopHook != nil {
		// run hook in routine to avoid blocking
		go m.stopHook()
	}
}

// this will not error, but it will block long enough for the master lock to be lost
func (m *master) Stop() error {
	if !m.isMaster.Val() {
		m.log.Debug("not currently the master, so nothing to stop")

		// this is not an error because the master is stopped
		// it just becomes a no-op
		return nil
	}

	// stop the heartbeat
	//TODO: if the heartbeat is not running, this will be a leak
	m.heartBeat.Quit()

	// attempt a release on the backend
	// this is a best effort. The heartbeat loop has been stopped,
	// so the lock will be lost eventually either way
	if err := m.lock.UnLock(m.uuid); err != nil {
		m.log.Errorf("failed to release lock on master backend: %v", err)
	}

	// at this point, as far as this node is concerned, it is
	// no longer the master. The only risk is that the heartbeat
	// did not quit properly
	// TODO: how can we determine that the heartbeat quit correctly?

	// this is only done once unlock is successful
	m.isMaster.SetFalse()
	m.info = &backend.MasterInfo{}

	return nil
}

func (m *master) IsMaster() bool {
	return m.isMaster.Val()
}

func (m *master) Status() (interface{}, error) {
	status := map[string]interface{}{
		"is_master": m.isMaster.Val(),
	}

	// currently there is nothing to make status error
	// eventually we could have something like error rate
	return status, nil
}

func (m *master) sendError(err error) {
	//if an err chan exists, send the error, otherwise log it
	if m.errors != nil {
		// do this in a routine in case no one is reading this channel
		// a routine leak is better than a lock up serving stale data
		// alternatively there could be an intermediate proxy that reads
		// this channel and queues up errors to be read. Then it becomes
		// a memory concern instead
		go func() {
			//TODO: implement deadline for send error routine
			m.errors <- err
		}()
	} else {
		m.log.Error(err)
	}
}

// a random uuid to use as a namespace
var nsUUID = uuid.Must(uuid.FromString("34b13033-50e7-4083-97f5-d389cf3a1c0e"))

// generate a UUID by iterating over the strategies, without throwing an error
func generateUUID() uuid.UUID {
	id, err := uuid.NewV1()
	if err != nil {
		id, err = uuid.NewV4()
		if err != nil {
			return uuid.NewV5(nsUUID, time.Now().String())
		}
	}

	return id
}
