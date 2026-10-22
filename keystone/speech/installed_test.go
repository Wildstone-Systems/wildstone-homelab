package speech

import (
	"context"
	"testing"
	"time"
)

func TestAnInstalledEngineTurnsLocalTranscriptionOn(t *testing.T) {
	f := &fakeEngine{installed: true, text: "hello"}
	r := &Router{AutoLocal: true}
	if r.Enabled() {
		t.Fatal("voice on with nothing installed")
	}
	r.SetInstalled(newEngine(t, f), nil)
	if !r.Available(false) {
		t.Fatal("an installed engine did not turn local transcription on")
	}
	if r.CanSpeak(true) {
		t.Fatal("an installed engine turned spoken replies on; they are a separate choice")
	}
	res, err := r.Transcribe(context.Background(), false, voice, "audio/ogg", 1)
	if err != nil || res.Text != "hello" || res.Processing != Local {
		t.Fatalf("Transcribe = %+v, %v", res, err)
	}
	if st := r.Status(); st.Mode != ModeLocal || st.Source != SourcePackage || st.State != ModelReady {
		t.Fatalf("status = %+v", st)
	}

	r.SetInstalled("", nil)
	if r.Enabled() {
		t.Fatal("voice stayed on after the engine was removed")
	}
}

func TestAnExplicitModeIsKept(t *testing.T) {
	f := &fakeEngine{installed: true, text: "hello"}
	r := &Router{Mode: ModeOff} // -speech-mode off: AutoLocal is false
	r.SetInstalled(newEngine(t, f), nil)
	if r.Enabled() {
		t.Fatal("an installed engine overrode -speech-mode off")
	}
}

func TestAnInstalledEngineFillsOnlyWhatTheFlagsLeftOpen(t *testing.T) {
	flag := &fakeEngine{installed: true, text: "from the flag"}
	pkg := &fakeEngine{installed: true, text: "from the package"}
	r := &Router{Mode: ModeLocal, Local: &Client{BaseURL: newEngine(t, flag), Where: Local}}
	r.SetInstalled(newEngine(t, pkg), nil)
	res, err := r.Transcribe(context.Background(), false, voice, "audio/ogg", 1)
	if err != nil || res.Text != "from the flag" {
		t.Fatalf("Transcribe = %+v, %v, want the -speech-url engine", res, err)
	}

	// -speech-replies local with no -speech-url: the installed engine
	// is the voice.
	r = &Router{ReplyMode: ModeLocal}
	r.SetInstalled(newEngine(t, pkg), nil)
	if !r.CanSpeak(false) {
		t.Fatal("-speech-replies local did not use the installed engine")
	}
	if r.Enabled() {
		t.Fatal("transcription turned on although -speech-mode was named")
	}
}

func TestTheModelIsFetchedAndReported(t *testing.T) {
	f := &fakeEngine{}
	r := &Router{AutoLocal: true}
	r.SetInstalled(newEngine(t, f), nil)
	deadline := time.Now().Add(5 * time.Second)
	for r.Status().State != ModelReady {
		if time.Now().After(deadline) {
			t.Fatalf("status = %+v, want ready", r.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAnUnreachableEngineIsStartingNotFailed(t *testing.T) {
	c := &Client{BaseURL: "http://127.0.0.1:1/v1", Model: "m", Where: Local, DownloadsModels: true}
	if err := c.download(context.Background()); err == nil {
		t.Fatal("download from nowhere succeeded")
	}
	if st, _ := c.modelState(); st != ModelStarting {
		t.Fatalf("state = %s, want starting", st)
	}
}
