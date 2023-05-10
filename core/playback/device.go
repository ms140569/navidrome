package playback

import (
	"context"
	"fmt"

	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
)

type PlaybackDevice struct {
	ParentPlaybackServer PlaybackServer
	Default              bool
	User                 string
	Name                 string
	Method               string
	DeviceName           string
	PlaybackQueue        *Queue
	Gain                 float32
	PlaybackDone         chan bool
	ActiveTrack          *Track
}

type DeviceStatus struct {
	CurrentIndex int
	Playing      bool
	Gain         float32
	Position     int
}

var EmptyStatus = DeviceStatus{CurrentIndex: -1, Playing: false, Gain: 0.5, Position: 0}

func (pd *PlaybackDevice) getStatus() DeviceStatus {
	pos := 0
	if pd.ActiveTrack != nil {
		pos = pd.ActiveTrack.Position()
	}
	return DeviceStatus{
		CurrentIndex: pd.PlaybackQueue.Index,
		Playing:      pd.isPlaying(),
		Gain:         pd.Gain,
		Position:     pos,
	}
}

// NewPlaybackDevice creates a new playback device which implements all the basic Jukebox mode commands defined here:
// http://www.subsonic.org/pages/api.jsp#jukeboxControl
// Starts the trackSwitcher goroutine for the device.
func NewPlaybackDevice(playbackServer PlaybackServer, name string, method string, deviceName string) *PlaybackDevice {
	playbackDevice := PlaybackDevice{
		ParentPlaybackServer: playbackServer,
		User:                 "",
		Name:                 name,
		Method:               method,
		DeviceName:           deviceName,
		Gain:                 0.5,
		PlaybackQueue:        NewQueue(),
		PlaybackDone:         make(chan bool),
	}

	// Start one trackSwitcher goroutine with each device
	go func() {
		playbackDevice.trackSwitcherGoroutine()
	}()

	return &playbackDevice
}

func (pd *PlaybackDevice) String() string {
	return fmt.Sprintf("Name: %s, Gain: %.4f, Loaded track: %s", pd.Name, pd.Gain, pd.ActiveTrack)
}

func (pd *PlaybackDevice) Get(ctx context.Context) (model.MediaFiles, DeviceStatus, error) {
	log.Debug(ctx, "processing Get action")
	return pd.PlaybackQueue.Get(), pd.getStatus(), nil
}

func (pd *PlaybackDevice) Status(ctx context.Context) (DeviceStatus, error) {
	log.Debug(ctx, fmt.Sprintf("processing Status action on: %s, queue: %s", pd, pd.PlaybackQueue))
	return pd.getStatus(), nil
}

// set is similar to a clear followed by a add, but will not change the currently playing track.
func (pd *PlaybackDevice) Set(ctx context.Context, ids []string) (DeviceStatus, error) {
	_, err := pd.Clear(ctx)
	if err != nil {
		log.Error(ctx, "error setting tracks", ids)
		return pd.getStatus(), err
	}
	return pd.Add(ctx, ids)
}

func (pd *PlaybackDevice) Start(ctx context.Context) (DeviceStatus, error) {
	log.Debug(ctx, "processing Start action")

	if pd.ActiveTrack != nil {
		if pd.isPlaying() {
			log.Debug("trying to start an already playing track")
		} else {
			pd.ActiveTrack.Unpause()
		}
	} else {
		pd.switchActiveTrackByIndex(ctx, pd.PlaybackQueue.Index)
		pd.ActiveTrack.Unpause()
	}

	return pd.getStatus(), nil
}

func (pd *PlaybackDevice) Stop(ctx context.Context) (DeviceStatus, error) {
	log.Debug(ctx, "processing Stop action")
	if pd.ActiveTrack != nil {
		pd.ActiveTrack.Pause()
	}
	return pd.getStatus(), nil
}

func (pd *PlaybackDevice) Skip(ctx context.Context, index int, offset int) (DeviceStatus, error) {
	log.Debug(ctx, "processing Skip action", "index", index, "offset", offset)

	wasPlaying := pd.isPlaying()

	if pd.ActiveTrack != nil && wasPlaying {
		pd.ActiveTrack.Pause()
	}

	if index != pd.PlaybackQueue.Index {
		pd.switchActiveTrackByIndex(ctx, index)
	}

	err := pd.PlaybackQueue.SetOffset(offset)
	if err != nil {
		log.Error(ctx, "error setting offset", err)
		return pd.getStatus(), err
	}

	if wasPlaying {
		_, err = pd.Start(ctx)
		if err != nil {
			log.Error(ctx, "error starting new track after skipping")
			return pd.getStatus(), err
		}
	}

	return pd.getStatus(), nil
}

func (pd *PlaybackDevice) Add(ctx context.Context, ids []string) (DeviceStatus, error) {
	log.Debug(ctx, "processing Add action")

	items := model.MediaFiles{}

	for _, id := range ids {
		mf, err := pd.ParentPlaybackServer.GetMediaFile(id)
		if err != nil {
			return DeviceStatus{}, err
		}
		log.Debug(ctx, "Found mediafile: "+mf.Path)
		items = append(items, *mf)
	}
	pd.PlaybackQueue.Add(items)

	return pd.getStatus(), nil
}

func (pd *PlaybackDevice) Clear(ctx context.Context) (DeviceStatus, error) {
	log.Debug(ctx, fmt.Sprintf("processing Clear action on: %s", pd))
	if pd.ActiveTrack != nil {
		pd.ActiveTrack.Pause()
		pd.ActiveTrack.Close()
		pd.ActiveTrack = nil
	}
	pd.PlaybackQueue.Clear()
	return pd.getStatus(), nil
}

func (pd *PlaybackDevice) Remove(ctx context.Context, index int) (DeviceStatus, error) {
	log.Debug(ctx, "processing Remove action")
	// pausing if attempting to remove running track
	if pd.isPlaying() && pd.PlaybackQueue.Index == index {
		_, err := pd.Stop(ctx)
		if err != nil {
			log.Error(ctx, "error stopping running track")
			return pd.getStatus(), err
		}
	}

	if index > -1 && index < pd.PlaybackQueue.Size() {
		pd.PlaybackQueue.Remove(index)
	} else {
		log.Error(ctx, "Index to remove out of range: "+fmt.Sprint(index))
	}
	return pd.getStatus(), nil
}

func (pd *PlaybackDevice) Shuffle(ctx context.Context) (DeviceStatus, error) {
	log.Debug(ctx, "processing Shuffle action")
	if pd.PlaybackQueue.Size() > 1 {
		pd.PlaybackQueue.Shuffle()
	}
	return pd.getStatus(), nil
}

func (pd *PlaybackDevice) SetGain(ctx context.Context, gain float32) (DeviceStatus, error) {
	difference := gain - pd.Gain
	log.Debug(ctx, fmt.Sprintf("processing SetGain action. Actual gain: %f, gain to set: %f, difference: %f", pd.Gain, gain, difference))

	if pd.ActiveTrack != nil {
		pd.ActiveTrack.SetVolume(float64(difference) * 5)
	}
	pd.Gain = gain

	return pd.getStatus(), nil
}

func (pd *PlaybackDevice) isPlaying() bool {
	return pd.ActiveTrack != nil && pd.ActiveTrack.IsPlaying()
}

func (pd *PlaybackDevice) trackSwitcherGoroutine() {
	log.Info("Starting trackSwitcher goroutine")
	for {
		<-pd.PlaybackDone
		log.Info("track switching detected")
		if pd.ActiveTrack != nil {
			pd.ActiveTrack.Close()
		}

		if !pd.PlaybackQueue.IsAtLastElement() {
			pd.PlaybackQueue.IncreaseIndex()
			log.Debug("Switching to next song", "queue", pd.PlaybackQueue.String())
			pd.switchActiveTrackByIndex(context.TODO(), pd.PlaybackQueue.Index)
			pd.ActiveTrack.Unpause()
		}
	}
}

func (pd *PlaybackDevice) switchActiveTrackByIndex(ctx context.Context, index int) error {
	pd.PlaybackQueue.SetIndex(index)
	currentTrack := pd.PlaybackQueue.Current()
	if currentTrack == nil {
		return fmt.Errorf("could not get current track")
	}

	err := pd.PlaybackQueue.SetOffset(0)
	if err != nil {
		return fmt.Errorf("error setting offset of next track to zero")
	}

	track, err := NewTrack(ctx, pd.PlaybackDone, *currentTrack)
	if err != nil {
		return err
	}
	pd.ActiveTrack = track
	return nil
}