package approval

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

const withdrawnLabelRunes = 80

// Withdraw moves the chosen pending records of the principal's waiting bundle, numbered as the user
// sees them, or all of them, to rejected with ReasonWithdrawn, and updates the messages that show
// them. Every number is checked before anything changes, so a bad one withdraws nothing. It
// returns the text for the model and whether anything was withdrawn.
func (s *Service) Withdraw(ctx context.Context, principalID string, items []int, all bool) (string, bool, error) {
	proposing := s.proposalLock(principalID)
	proposing.Lock()
	defer proposing.Unlock()
	pending, err := s.repository.PendingApprovals(ctx, principalID)
	if err != nil {
		return "", false, err
	}
	if len(pending) == 0 {
		return "No proposal of yours is waiting for the user's confirmation; nothing was withdrawn.", false, nil
	}
	bundle, err := s.repository.LoadBundle(ctx, pending[0].BundleID)
	if err != nil {
		return "", false, err
	}
	bundle = shown(bundle)

	var chosen []int
	if all {
		for index, record := range bundle {
			if record.Status == StatusPending {
				chosen = append(chosen, index)
			}
		}
	}
	for _, number := range items {
		if number < 1 || number > len(bundle) {
			return fmt.Sprintf("There is no item %d; the batch has %d. Nothing was withdrawn.", number, len(bundle)), false, nil
		}
		if record := bundle[number-1]; record.Status != StatusPending {
			return fmt.Sprintf("Item %d is no longer waiting (%s), so it cannot be withdrawn. Nothing was withdrawn.", number, statusLine(record, bundleOutcomeRunes)), false, nil
		}
		if !slices.Contains(chosen, number-1) {
			chosen = append(chosen, number-1)
		}
	}
	slices.Sort(chosen)

	var withdrawn, missed []string
	var records []Record
	for _, index := range chosen {
		record := &bundle[index]
		finished, err := s.repository.FinishApproval(ctx, record.ID, StatusPending, StatusRejected, ReasonWithdrawn, "")
		if err != nil {
			return "", false, err
		}
		label := fmt.Sprintf("%d (%s)", index+1, itemLabel(*record))
		if !finished {
			missed = append(missed, label)
			continue
		}
		record.Status, record.Reason = StatusRejected, ReasonWithdrawn
		records = append(records, *record)
		withdrawn = append(withdrawn, label)
		s.recordStatus(ctx, *record, ReasonWithdrawn)
		s.logger.InfoContext(ctx, "approval.resolved", "principal_id", principalID, "approval_id", record.ID, "bundle_id", record.BundleID, "status", record.Status, "reason", record.Reason)
	}
	s.dropIntents(records)
	delivered := bundle[len(bundle)-1].MessageID != 0
	if delivered && len(records) > 0 {
		s.showWithdrawal(ctx, bundle, records)
	}

	var text strings.Builder
	if len(withdrawn) > 0 {
		text.WriteString("Withdrew item " + strings.Join(withdrawn, ", item ") + ". ")
	}
	if len(missed) > 0 {
		text.WriteString("Item " + strings.Join(missed, ", item ") + " was resolved by the user meanwhile and was not withdrawn. ")
	}
	if delivered && len(records) > 0 {
		text.WriteString("The user's proposal message now shows what was withdrawn. ")
	}
	var waiting []string
	for index, record := range shown(bundle) {
		if record.Status == StatusPending {
			waiting = append(waiting, fmt.Sprintf("%d. %s", index+1, itemLabel(record)))
		}
	}
	if len(waiting) == 0 {
		text.WriteString("Nothing is waiting for the user's confirmation any more.")
	} else {
		text.WriteString("Still waiting for the user's confirmation, numbered as the user sees them: " + strings.Join(waiting, "; "))
	}
	return text.String(), len(withdrawn) > 0, nil
}

// showWithdrawal edits the messages that show the withdrawn records. The message with the buttons
// keeps them while anything is still waiting, and loses them once nothing is.
func (s *Service) showWithdrawal(ctx context.Context, bundle []Record, withdrawn []Record) {
	last := bundle[len(bundle)-1]
	edited := map[int64]bool{last.MessageID: true}
	for _, record := range withdrawn {
		if edited[record.MessageID] || record.MessageID == 0 {
			continue
		}
		edited[record.MessageID] = true
		from := slices.IndexFunc(bundle, func(item Record) bool { return item.MessageID == record.MessageID })
		to := from
		for to < len(bundle) && bundle[to].MessageID == record.MessageID {
			to++
		}
		if err := s.deliverer.EditText(ctx, record.ChatID, record.MessageID, messageText(bundle, from, to)); err != nil {
			s.logger.WarnContext(ctx, "approval.withdrawal.unshown", "principal_id", record.PrincipalID, "bundle_id", record.BundleID, "error", err)
		}
	}
	var err error
	if slices.ContainsFunc(bundle, func(record Record) bool { return record.Status == StatusPending }) {
		err = s.deliverer.EditProposal(ctx, last.ChatID, last.MessageID, bundleText(bundle), s.buttons(ctx, bundle))
	} else {
		err = s.deliverer.EditText(ctx, last.ChatID, last.MessageID, bundleText(bundle))
	}
	if err != nil {
		s.logger.WarnContext(ctx, "approval.withdrawal.unshown", "principal_id", last.PrincipalID, "bundle_id", last.BundleID, "error", err)
	}
}

// itemLabel names a proposal for the model by its tool and the first line of its summary.
func itemLabel(record Record) string {
	summary := strings.TrimPrefix(record.Summary, IntentWarning+"\n\n")
	line, _, _ := strings.Cut(summary, "\n")
	return record.Tool + ": " + truncateRunes(line, withdrawnLabelRunes)
}
