package wiring

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/vladar107/home-ai-assistant/internal/export"
	"github.com/vladar107/home-ai-assistant/internal/parcel"
	"github.com/vladar107/home-ai-assistant/internal/thread"
	"github.com/vladar107/home-ai-assistant/internal/turn"
)

// parcelSupport is what the inbox check and its chat guidance need of Parcels: the shared proposal
// check, and whether the principal has Parcels on.
type parcelSupport struct {
	parcel.Proposals
	enabler parcel.SwitchEnabler
}

func (p parcelSupport) Enabled(ctx context.Context, principalID string) (bool, error) {
	return p.enabler.Enabled(ctx, principalID)
}

// hasParcelEntry says whether the principal has an assigned Home Assistant entry of the domain. The
// sources know their entries' domains only after a read, which the cache makes cheap. Without Home
// Assistant it is false.
func hasParcelEntry(ctx context.Context, home parcel.HomeSource, principalID, domain string) bool {
	if home == nil {
		return false
	}
	_, _ = home.Read(ctx, principalID)
	sources, err := home.List(ctx, principalID)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(sources, func(source parcel.SourceInfo) bool {
		return source.Domain == domain && source.State != parcel.SourceMissing
	})
}

// parcelExtractor runs the mail job's toolless extraction on the engine.
type parcelExtractor struct{ engine *turn.Engine }

func (e parcelExtractor) RunExtract(ctx context.Context, principalID, message string) (string, string, error) {
	reply, err := e.engine.RunExtract(ctx, principalID, message)
	return reply.Text, reply.ErrorKind, err
}

// parcelDestination resolves the chat and Parcels tab the mail job's proposals are shown in.
type parcelDestination struct {
	chats interface {
		ResolveActiveChat(ctx context.Context, principalID string) (int64, bool, error)
	}
	feeds thread.Feeds
}

func (d parcelDestination) ParcelView(ctx context.Context, principalID string) (int64, int64, bool, error) {
	chatID, found, err := d.chats.ResolveActiveChat(ctx, principalID)
	if err != nil || !found {
		return 0, 0, false, err
	}
	threadID, err := d.feeds.Thread(ctx, principalID, chatID, thread.FeedParcels)
	if err != nil {
		return 0, 0, false, err
	}
	return chatID, threadID, true, nil
}

// ParcelMail tells the Parcels settings page whether the principal's mail can be read.
type ParcelMail struct{ state *tripScanState }

// Readable reports whether the principal has a Google account whose mail can be read.
func (m ParcelMail) Readable(ctx context.Context, principalID string) (bool, error) {
	return m.state.MailReadable(ctx, principalID)
}

// parcelExporter is the parcels section of an export, which starts the loop's silent baseline once
// a restore added rows, so the restored parcels get new cards without notices.
type parcelExporter struct {
	export.Exporter
	loop *parcel.Loop
	a    *Assembly
}

func (e parcelExporter) Restored(ctx context.Context, principalID string) {
	if err := e.loop.Baseline(ctx, principalID); err != nil {
		e.a.in.Logger.WarnContext(ctx, "parcels.baseline_after_restore_failed", "principal_id", principalID, "error", fmt.Sprintf("%T", err))
	}
}

// buildParcels wires Parcels once the connectors it reads exist: the Home Assistant source for the
// entries the operator assigned, the mail source, the loop that keeps the cards current, and the
// tools. It does nothing when the tabs do not offer parcel cards.
func (a *Assembly) buildParcels() error {
	in := a.in
	if a.tabs.Parcels == nil {
		if len(in.ParcelSources) > 0 {
			return errors.New("wiring: parcel sources are assigned but parcel cards are not offered")
		}
		return nil
	}
	for principalID := range in.ParcelSources {
		if !slices.Contains(a.principals, principalID) {
			return fmt.Errorf("HOME_AGENT_PARCEL_SOURCES: principal %q is not on the Telegram allowlist", principalID)
		}
	}
	// A typed nil would hide a missing connector, so each is set only when it exists.
	var home parcel.HomeSource
	var sources parcel.Sources
	var untracker parcel.Untracker
	var accounts parcel.Accounts
	switch {
	case len(in.ParcelSources) > 0 && a.HomeAssistant != nil:
		source, err := a.HomeAssistant.NewParcelSource(in.ParcelSources, in.Store)
		if err != nil {
			return fmt.Errorf("HOME_AGENT_PARCEL_SOURCES: %w", err)
		}
		home, sources, untracker, accounts = source, source, source, source
	case len(in.ParcelSources) > 0:
		return errors.New("HOME_AGENT_PARCEL_SOURCES: Home Assistant is not configured")
	}
	proposals := parcel.Proposals{
		Repository: in.Store, Approvals: in.Store, Carriers: parcel.DefaultCarriers(), Now: in.Core.Now,
		HasEntry: func(ctx context.Context, principalID, domain string) bool {
			return hasParcelEntry(ctx, home, principalID, domain)
		},
	}
	a.Intake.SetParcels(parcelSupport{Proposals: proposals, enabler: parcel.SwitchEnabler{Reader: a.Switches}})
	var mail parcel.MailRunner
	if a.gmail != nil {
		source := &parcel.MailSource{
			Repository: in.Store, Mailboxes: a.gmail, Mail: a.tripState, Extractor: parcelExtractor{engine: a.Turns}, Carriers: parcel.DefaultCarriers(),
			Proposals: proposals, Offers: a.Approvals, Destination: parcelDestination{chats: in.Store, feeds: a.tabs.Feeds},
			Accounts: accounts, Location: a.location, Logger: in.Logger,
		}
		mail = source
	}
	loop, err := parcel.NewLoop(parcel.LoopConfig{
		Repository: in.Store, Sources: sources, Mail: mail, Untracker: untracker, Cards: a.tabs.Parcels,
		Enabled: parcel.SwitchEnabler{Reader: a.Switches}, Principals: func() []string { return slices.Clone(a.principals) },
		Now: in.Core.Now, Location: a.location, Languages: in.Store, Logger: in.Logger,
	})
	if err != nil {
		return fmt.Errorf("initialize parcel loop: %w", err)
	}
	if err := a.Registry.Register(loop); err != nil {
		return err
	}
	tools, err := parcel.NewTools(parcel.ToolsConfig{
		Repository: in.Store, Proposals: proposals, Actor: a.Approvals, Home: home, Tracker: loop,
		Location: a.location, Now: in.Core.Now, Logger: in.Logger,
	})
	if err != nil {
		return fmt.Errorf("initialize parcel tools: %w", err)
	}
	if err := a.Tools.Register(tools); err != nil {
		return err
	}
	if err := a.Approvals.RegisterExecutor(tools); err != nil {
		return err
	}
	a.ParcelLoop, a.ParcelSources, a.ParcelTools = loop, sources, tools
	return nil
}

// ParcelMailState tells the Parcels settings page whether the principal's mail can be read.
func (a *Assembly) ParcelMailState() ParcelMail { return ParcelMail{state: a.tripState} }
