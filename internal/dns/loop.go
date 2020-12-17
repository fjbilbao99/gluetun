package dns

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/qdm12/gluetun/internal/constants"
	"github.com/qdm12/gluetun/internal/settings"
	"github.com/qdm12/golibs/command"
	"github.com/qdm12/golibs/logging"
)

type Looper interface {
	Run(ctx context.Context, wg *sync.WaitGroup, signalDNSReady func())
	RunRestartTicker(ctx context.Context, wg *sync.WaitGroup)
	Restart()
	Start()
	Stop()
	GetSettings() (settings settings.DNS)
	SetSettings(settings settings.DNS)
}

type looper struct {
	conf          Configurator
	settings      settings.DNS
	settingsMutex sync.RWMutex
	logger        logging.Logger
	streamMerger  command.StreamMerger
	uid           int
	gid           int
	localSubnet   net.IPNet
	restart       chan struct{}
	start         chan struct{}
	stop          chan struct{}
	updateTicker  chan struct{}
	timeNow       func() time.Time
	timeSince     func(time.Time) time.Duration
}

func NewLooper(conf Configurator, settings settings.DNS, logger logging.Logger,
	streamMerger command.StreamMerger, uid, gid int, localSubnet net.IPNet) Looper {
	return &looper{
		conf:         conf,
		settings:     settings,
		logger:       logger.WithPrefix("dns over tls: "),
		uid:          uid,
		gid:          gid,
		localSubnet:  localSubnet,
		streamMerger: streamMerger,
		restart:      make(chan struct{}),
		start:        make(chan struct{}),
		stop:         make(chan struct{}),
		updateTicker: make(chan struct{}),
		timeNow:      time.Now,
		timeSince:    time.Since,
	}
}

func (l *looper) Restart() { l.restart <- struct{}{} }
func (l *looper) Start()   { l.start <- struct{}{} }
func (l *looper) Stop()    { l.stop <- struct{}{} }

func (l *looper) GetSettings() (settings settings.DNS) {
	l.settingsMutex.RLock()
	defer l.settingsMutex.RUnlock()
	return l.settings
}

func (l *looper) SetSettings(settings settings.DNS) {
	l.settingsMutex.Lock()
	defer l.settingsMutex.Unlock()
	updatePeriodDiffers := l.settings.UpdatePeriod != settings.UpdatePeriod
	l.settings = settings
	l.settingsMutex.Unlock()
	if updatePeriodDiffers {
		l.updateTicker <- struct{}{}
	}
}

func (l *looper) isEnabled() bool {
	l.settingsMutex.RLock()
	defer l.settingsMutex.RUnlock()
	return l.settings.Enabled
}

func (l *looper) setEnabled(enabled bool) {
	l.settingsMutex.Lock()
	defer l.settingsMutex.Unlock()
	l.settings.Enabled = enabled
}

func (l *looper) logAndWait(ctx context.Context, err error) {
	l.logger.Warn(err)
	l.logger.Info("attempting restart in 10 seconds")
	const waitDuration = 10 * time.Second
	timer := time.NewTimer(waitDuration)
	select {
	case <-timer.C:
	case <-ctx.Done():
		if !timer.Stop() {
			<-timer.C
		}
	}
}

func (l *looper) waitForFirstStart(ctx context.Context, signalDNSReady func()) {
	for {
		select {
		case <-l.stop:
			l.setEnabled(false)
			l.logger.Info("not started yet")
		case <-l.restart:
			if l.isEnabled() {
				return
			}
			signalDNSReady()
			l.logger.Info("not restarting because disabled")
		case <-l.start:
			l.setEnabled(true)
			return
		case <-ctx.Done():
			return
		}
	}
}

func (l *looper) waitForSubsequentStart(ctx context.Context, unboundCancel context.CancelFunc) {
	if l.isEnabled() {
		return
	}
	for {
		// wait for a signal to re-enable
		select {
		case <-l.stop:
			l.logger.Info("already disabled")
		case <-l.restart:
			if !l.isEnabled() {
				l.logger.Info("not restarting because disabled")
			} else {
				return
			}
		case <-l.start:
			l.setEnabled(true)
			return
		case <-ctx.Done():
			unboundCancel()
			return
		}
	}
}

func (l *looper) Run(ctx context.Context, wg *sync.WaitGroup, signalDNSReady func()) {
	defer wg.Done()
	const fallback = false
	l.useUnencryptedDNS(fallback)
	l.waitForFirstStart(ctx, signalDNSReady)
	if ctx.Err() != nil {
		return
	}
	defer l.logger.Warn("loop exited")

	var unboundCtx context.Context
	var unboundCancel context.CancelFunc = func() {}
	var waitError chan error
	triggeredRestart := false
	l.setEnabled(true)
	for ctx.Err() == nil {
		l.waitForSubsequentStart(ctx, unboundCancel)

		settings := l.GetSettings()

		// Setup
		if err := l.conf.DownloadRootHints(ctx, l.uid, l.gid); err != nil {
			l.logAndWait(ctx, err)
			continue
		}
		if err := l.conf.DownloadRootKey(ctx, l.uid, l.gid); err != nil {
			l.logAndWait(ctx, err)
			continue
		}
		if err := l.conf.MakeUnboundConf(ctx, settings, l.localSubnet, l.uid, l.gid); err != nil {
			l.logAndWait(ctx, err)
			continue
		}

		if triggeredRestart {
			triggeredRestart = false
			unboundCancel()
			<-waitError
			close(waitError)
		}
		unboundCtx, unboundCancel = context.WithCancel(context.Background())
		stream, waitFn, err := l.conf.Start(unboundCtx, settings.VerbosityDetailsLevel)
		if err != nil {
			unboundCancel()
			const fallback = true
			l.useUnencryptedDNS(fallback)
			l.logAndWait(ctx, err)
			continue
		}

		// Started successfully
		go l.streamMerger.Merge(unboundCtx, stream, command.MergeName("unbound"))
		l.conf.UseDNSInternally(net.IP{127, 0, 0, 1})                                                  // use Unbound
		if err := l.conf.UseDNSSystemWide(net.IP{127, 0, 0, 1}, settings.KeepNameserver); err != nil { // use Unbound
			l.logger.Error(err)
		}
		if err := l.conf.WaitForUnbound(); err != nil {
			unboundCancel()
			const fallback = true
			l.useUnencryptedDNS(fallback)
			l.logAndWait(ctx, err)
			continue
		}
		waitError = make(chan error)
		go func() {
			err := waitFn() // blocking
			waitError <- err
		}()
		l.logger.Info("DNS over TLS is ready")
		signalDNSReady()

		stayHere := true
		for stayHere {
			select {
			case <-ctx.Done():
				l.logger.Warn("context canceled: exiting loop")
				unboundCancel()
				<-waitError
				close(waitError)
				return
			case <-l.restart: // triggered restart
				l.logger.Info("restarting")
				// unboundCancel occurs next loop run when the setup is complete
				triggeredRestart = true
				stayHere = false
			case <-l.start:
				l.logger.Info("already started")
			case <-l.stop:
				l.logger.Info("stopping")
				unboundCancel()
				<-waitError
				close(waitError)
				l.setEnabled(false)
				stayHere = false
			case err := <-waitError: // unexpected error
				close(waitError)
				unboundCancel()
				const fallback = true
				l.useUnencryptedDNS(fallback)
				l.logAndWait(ctx, err)
				stayHere = false
			}
		}
	}
	unboundCancel()
}

func (l *looper) useUnencryptedDNS(fallback bool) {
	settings := l.GetSettings()

	// Try with user provided plaintext ip address
	targetIP := settings.PlaintextAddress
	if targetIP != nil {
		if fallback {
			l.logger.Info("falling back on plaintext DNS at address %s", targetIP)
		} else {
			l.logger.Info("using plaintext DNS at address %s", targetIP)
		}
		l.conf.UseDNSInternally(targetIP)
		if err := l.conf.UseDNSSystemWide(targetIP, settings.KeepNameserver); err != nil {
			l.logger.Error(err)
		}
		return
	}

	// Try with any IPv4 address from the providers chosen
	for _, provider := range settings.Providers {
		data := constants.DNSProviderMapping()[provider]
		for _, targetIP = range data.IPs {
			if targetIP.To4() != nil {
				l.logger.Info("falling back on plaintext DNS at address %s", targetIP)
				l.conf.UseDNSInternally(targetIP)
				if err := l.conf.UseDNSSystemWide(targetIP, settings.KeepNameserver); err != nil {
					l.logger.Error(err)
				}
				return
			}
		}
	}

	// No IPv4 address found
	l.logger.Error("no ipv4 DNS address found for providers %s", settings.Providers)
}

func (l *looper) RunRestartTicker(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	// Timer that acts as a ticker
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	timerIsStopped := true
	settings := l.GetSettings()
	if settings.UpdatePeriod > 0 {
		timer.Reset(settings.UpdatePeriod)
		timerIsStopped = false
	}
	lastTick := time.Unix(0, 0)
	for {
		select {
		case <-ctx.Done():
			if !timerIsStopped && !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
			lastTick = l.timeNow()
			l.restart <- struct{}{}
			settings := l.GetSettings()
			timer.Reset(settings.UpdatePeriod)
		case <-l.updateTicker:
			if !timer.Stop() {
				<-timer.C
			}
			timerIsStopped = true
			settings := l.GetSettings()
			newUpdatePeriod := settings.UpdatePeriod
			if newUpdatePeriod == 0 {
				continue
			}
			var waited time.Duration
			if lastTick.UnixNano() != 0 {
				waited = l.timeSince(lastTick)
			}
			leftToWait := newUpdatePeriod - waited
			timer.Reset(leftToWait)
			timerIsStopped = false
		}
	}
}
