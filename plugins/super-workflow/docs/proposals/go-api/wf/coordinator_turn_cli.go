package wf

import (
	"flag"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
)

func turnAttachRow() Subcommand {
	return Subcommand{Name: "attach-coordinator", Summary: "select this named session to coordinate the run",
		Flags:  []string{"--state-dir dir", "--assets dir", "--workspace dir", "--initial", "--previous-run ID", "--previous-state-dir DIR"},
		handle: (*cli).drive, subject: "run id", act: attachTurnCLI,
		define: func(fs *flag.FlagSet, f *flags) {
			fs.BoolVar(&f.turnAttach.initial, "initial", false, "assert this native session has never been attached")
			fs.Func("previous-run", "previous run ID after loss of the local selection", func(value string) error {
				run := RunID(value)
				if err := validRunID(run); err != nil {
					return err
				}
				f.turnAttach.previousRun = run
				return nil
			})
			fs.StringVar(&f.turnAttach.previousState, "previous-state-dir", "", "absolute previous state directory after loss of the local selection")
		}}
}

func turnQueryRow() Subcommand {
	return Subcommand{Name: "may-end-turn", Summary: "read the committed coordinator stop decision",
		Flags: []string{"--state-dir dir", "--assets dir", "--workspace dir", "--json"}, handle: (*cli).drive, subject: "run id",
		define: func(fs *flag.FlagSet, f *flags) { fs.BoolVar(&f.json, "json", false, "print the decision as JSON") },
		act:    queryTurnCLI}
}

func turnHandoffRow() Subcommand {
	return Subcommand{Name: "acknowledge-handoff", Summary: "acknowledge a current person handoff",
		Flags: []string{"--state-dir dir", "--assets dir", "--workspace dir", "--receipt ID"}, handle: (*cli).drive, subject: "run id",
		define: func(fs *flag.FlagSet, f *flags) { fs.Int64Var(&f.receipt, "receipt", 0, "handoff receipt sequence") },
		act: func(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int {
			if f.receipt <= 0 {
				return c.failf(exitUsage, "%s: --receipt is required", row.Name)
			}
			return c.withEngine(row, stateDir, func(x Executor) int {
				if err := x.(*Engine).acknowledgeHandoff(run, envCoordinator(c.env), f.receipt); err != nil {
					return c.fromError(row.Name, err)
				}
				return exitOK
			})
		}}
}

func attachTurnCLI(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int {
	opts := f.turnAttach
	if (opts.previousState == "") != (opts.previousRun == "") ||
		(opts.previousRun != "" && (opts.initial || !filepath.IsAbs(opts.previousState))) {
		return c.failf(exitUsage, "%s: pass both previous-run and an absolute previous-state-dir, or use --initial", row.Name)
	}
	return c.withEngine(row, stateDir, func(x Executor) int {
		e, co := x.(*Engine), envCoordinator(c.env)
		generation, err := c.attachTurnBinding(e, stateDir, run, co, opts)
		if err != nil {
			return c.fromError(row.Name, err)
		}
		fmt.Fprintln(c.stdout, generation)
		return exitOK
	})
}

func queryTurnCLI(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int {
	return c.withEngine(row, stateDir, func(x Executor) int {
		e := x.(*Engine)
		d, err := e.MayEndTurn(run, envCoordinator(c.env))
		d.command(e.opts.caller.Executable, stateDir, run, c.assets, c.cfg.named())
		if f.json {
			if code := c.printJSON(d); code != exitOK {
				return code
			}
		} else {
			fmt.Fprintf(c.stdout, "%s %s\n", d.Decision, d.Reason)
		}
		if err != nil {
			return c.fromError(row.Name, err)
		}
		if d.Decision == "unavailable" {
			return exitRejected
		}
		return exitOK
	})
}

func (c *cycle) prepareTurnHandoff(ins []Instruction) error {
	t := c.st.turn
	if t == nil || t.generation == 0 || !matchingAttachment(t.attachment, c.caller) {
		return nil
	}
	d := CoordinatorHandoffData{Admission: t.generation, WaitingMaps: maps.Clone(c.turnWaitingMaps)}
	for _, instruction := range ins {
		in := c.st.insts[instruction.Child]
		if in == nil {
			return c.invalidateTurnHandoff()
		}
		frontier := CoordinatorHandoffFrontier{Instance: in.ID, State: in.Cur}
		switch {
		case instruction.Kind == InstructionHumanGate && !instruction.Delegated && !instruction.CoordinatorAnswers,
			instruction.Kind == InstructionStopped && instruction.Question != "" && !instruction.CoordinatorAnswers:
			frontier.Visit = instruction.Visit
		case instruction.Kind == InstructionManualVerification && instruction.Person && !instruction.Transient:
			frontier.Effect = instruction.Effect
		default:
			return c.invalidateTurnHandoff()
		}
		d.Frontiers = append(d.Frontiers, frontier)
	}
	if !c.turnHandoffCurrent(coordinatorHandoff{data: d, seq: c.nextSeq()}) {
		return c.invalidateTurnHandoff()
	}
	prior := t.handoff.data
	if t.handoff.seq == 0 || prior.Admission != d.Admission || !slices.Equal(prior.Frontiers, d.Frontiers) ||
		!maps.Equal(prior.WaitingMaps, d.WaitingMaps) {
		if err := c.emitAs("", "", 0, JournalCoordinatorHandoff, 0, d); err != nil {
			return err
		}
	}
	for i := range ins {
		ins[i].HandoffReceipt = t.handoff.seq
	}
	if !t.handoff.acknowledged {
		ins[len(ins)-1].Prompt += "\nAfter showing the person's requests, record receipt before ending your turn:\n" +
			c.verbLine("acknowledge-handoff", "--receipt", strconv.FormatInt(t.handoff.seq, 10))
	}
	return nil
}

func (e *Engine) acknowledgeHandoff(run RunID, co Coordinator, receipt int64) error {
	return e.withTelemetry(run, func(c *cycle) error {
		t := c.st.turn
		if t == nil || t.generation == 0 || c.st.paused || c.st.ended || !matchingAttachment(t.attachment, co) ||
			t.handoff.seq != receipt || !c.turnHandoffCurrent(t.handoff) {
			return fmt.Errorf("%w: handoff receipt is stale or belongs to another attachment", ErrRejected)
		}
		if err := c.checkTurnDefinition(); err != nil {
			return err
		}
		if t.handoff.acknowledged {
			return nil
		}
		d := t.handoff.data
		d.Receipt = receipt
		return c.emitAs("", "", 0, JournalCoordinatorHandoff, 0, d)
	})
}

func (c *cycle) invalidateTurnHandoff() error {
	t := c.st.turn
	if t.handoff.seq == 0 {
		return nil
	}
	d := t.handoff.data
	d.Receipt, d.Invalidated = t.handoff.seq, true
	return c.emitAs("", "", 0, JournalCoordinatorHandoff, 0, d)
}
