package main

import (
	"bytes"
	"fmt"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
)

const (
	cvLoadRefreshMs = 500
	cvColumnNum     = 4
	cvDateFormat    = "2006-01-02 15:04"
)

type commitViewHandler func(*CommitView, Action) error

type loadingCommitsRefreshTask struct {
	refreshRate time.Duration
	ticker      *time.Ticker
	channels    *Channels
	cancelCh    chan<- bool
}

type referenceViewData struct {
	viewPos        ViewPos
	tableFormatter *TableFormatter
}

// CommitViewListener is notified when a commit is selected
type CommitViewListener interface {
	OnCommitSelected(*Commit) error
}

// CommitView is the overall instance representing the commit view
type CommitView struct {
	channels            *Channels
	repoData            RepoData
	activeRef           Ref
	active              bool
	refViewData         map[string]*referenceViewData
	handlers            map[ActionType]commitViewHandler
	refreshTask         *loadingCommitsRefreshTask
	commitViewListeners []CommitViewListener
	viewDimension       ViewDimension
	viewSearch          *ViewSearch
	lock                sync.Mutex
}

// NewCommitView creates a new instance of the commit view
func NewCommitView(repoData RepoData, channels *Channels) *CommitView {
	commitView := &CommitView{
		channels:    channels,
		repoData:    repoData,
		refViewData: make(map[string]*referenceViewData),
		handlers: map[ActionType]commitViewHandler{
			ActionPrevLine:     moveUpCommit,
			ActionNextLine:     moveDownCommit,
			ActionPrevPage:     moveUpCommitPage,
			ActionNextPage:     moveDownCommitPage,
			ActionScrollRight:  scrollCommitViewRight,
			ActionScrollLeft:   scrollCommitViewLeft,
			ActionFirstLine:    moveToFirstCommit,
			ActionLastLine:     moveToLastCommit,
			ActionAddFilter:    addCommitFilter,
			ActionRemoveFilter: removeCommitFilter,
			ActionCenterView:   centerCommitView,
			ActionSelect:       selectCommit,
		},
	}

	commitView.viewSearch = NewViewSearch(commitView, channels)

	return commitView
}

// Initialise currently does nothing
func (commitView *CommitView) Initialise() (err error) {
	log.Info("Initialising CommitView")

	commitView.repoData.RegisterCommitSetListener(commitView)

	return
}

// Render generates and draws the commit view to the provided window
func (commitView *CommitView) Render(win RenderWindow) (err error) {
	log.Debug("Rendering CommitView")
	commitView.lock.Lock()
	defer commitView.lock.Unlock()

	commitView.viewDimension = win.ViewDimensions()

	if commitView.activeRef == nil {
		return commitView.renderEmptyView(win)
	}

	refViewData, ok := commitView.refViewData[commitView.activeRef.Name()]
	if !ok {
		return fmt.Errorf("No RefViewData exists for ref %v", commitView.activeRef.Name())
	}

	commitSetState := commitView.repoData.CommitSetState(commitView.activeRef)
	commitNum := commitSetState.commitNum

	viewPos := refViewData.viewPos
	rows := win.Rows() - 2
	viewPos.DetermineViewStartRow(rows, commitNum)

	commitDisplayNum := rows
	startCommitIndex := viewPos.ViewStartRowIndex()

	commitCh, err := commitView.repoData.Commits(commitView.activeRef, startCommitIndex, commitDisplayNum)
	if err != nil {
		return err
	}

	tableFormatter := refViewData.tableFormatter
	tableFormatter.Resize(rows)
	tableFormatter.Clear()

	rowIndex := uint(0)

	for commit := range commitCh {
		if err = commitView.renderCommit(tableFormatter, rowIndex, commit); err != nil {
			return
		}

		rowIndex++
	}

	if err = tableFormatter.Render(win, viewPos.ViewStartColumn(), true); err != nil {
		return
	}

	if commitSetState.commitNum > 0 {
		if err = win.SetSelectedRow(viewPos.SelectedRowIndex()+1, commitView.active); err != nil {
			return
		}
	}

	win.DrawBorder()

	if err = win.SetTitle(CmpCommitviewTitle, "Commits for %v", commitView.activeRef.Shorthand()); err != nil {
		return
	}

	var selectedCommit uint
	if commitSetState.commitNum == 0 {
		selectedCommit = 0
	} else {
		selectedCommit = viewPos.ActiveRowIndex() + 1
	}

	var footerText bytes.Buffer

	footerText.WriteString(fmt.Sprintf("Commit %v of %v", selectedCommit, commitSetState.commitNum))

	if commitSetState.filterState != nil {
		filtersApplied := commitSetState.filterState.filtersApplied
		filtersTextSuffix := ""

		if filtersApplied > 1 {
			filtersTextSuffix = "s"
		}

		footerText.WriteString(fmt.Sprintf(" (%v filter%v applied)", commitSetState.filterState.filtersApplied, filtersTextSuffix))
	}

	if err = win.SetFooter(CmpCommitviewFooter, "%v", footerText.String()); err != nil {
		return
	}

	if searchActive, searchPattern, lastSearchFoundMatch := commitView.viewSearch.SearchActive(); searchActive && lastSearchFoundMatch {
		if err = win.Highlight(searchPattern, CmpAllviewSearchMatch); err != nil {
			return
		}
	}

	return err
}

func (commitView *CommitView) renderEmptyView(win RenderWindow) (err error) {
	if err = win.SetRow(2, 1, CmpNone, "   No commits to display"); err != nil {
		return
	}

	win.DrawBorder()

	return
}

func (commitView *CommitView) renderCommit(tableFormatter *TableFormatter, rowIndex uint, commit *Commit) (err error) {
	author := commit.commit.Author()
	commitRefs := commitView.repoData.RefsForCommit(commit)
	colIndex := uint(0)

	if err = tableFormatter.SetCellWithStyle(rowIndex, colIndex, CmpCommitviewShortOid, "%v", commit.oid.ShortID()); err != nil {
		return
	}

	colIndex++
	if err = tableFormatter.SetCellWithStyle(rowIndex, colIndex, CmpCommitviewDate, "%v", author.When.Format(cvDateFormat)); err != nil {
		return
	}

	colIndex++
	if err = tableFormatter.SetCellWithStyle(rowIndex, colIndex, CmpCommitviewAuthor, "%v", author.Name); err != nil {
		return
	}

	colIndex++
	if len(commitRefs.tags) > 0 {
		for _, tag := range commitRefs.tags {
			if err = tableFormatter.AppendToCellWithStyle(rowIndex, colIndex, CmpCommitviewTag, "<%v>", tag.Shorthand()); err != nil {
				return
			}

			if err = tableFormatter.AppendToCell(rowIndex, colIndex, " "); err != nil {
				return
			}
		}
	}

	if len(commitRefs.branches) > 0 {
		for _, branch := range commitRefs.branches {
			if branch.IsRemote() {
				if err = tableFormatter.AppendToCellWithStyle(rowIndex, colIndex, CmpCommitviewLocalBranch, "{%v}", branch.Shorthand()); err != nil {
					return
				}
			} else {
				if err = tableFormatter.AppendToCellWithStyle(rowIndex, colIndex, CmpCommitviewRemoteBranch, "[%v]", branch.Shorthand()); err != nil {
					return
				}
			}

			if err = tableFormatter.AppendToCell(rowIndex, colIndex, " "); err != nil {
				return
			}
		}
	}

	if err = tableFormatter.AppendToCellWithStyle(rowIndex, colIndex, CmpCommitviewSummary, "%v", commit.commit.Summary()); err != nil {
		return
	}

	return
}

// RenderHelpBar shows key bindings custom to the commit view
func (commitView *CommitView) RenderHelpBar(lineBuilder *LineBuilder) (err error) {
	RenderKeyBindingHelp(commitView.ViewID(), lineBuilder, []ActionMessage{
		{action: ActionFilterPrompt, message: "Add Filter"},
		{action: ActionRemoveFilter, message: "Remove Filter"},
	})

	return
}

func newLoadingCommitsRefreshTask(refreshRate time.Duration, channels *Channels) *loadingCommitsRefreshTask {
	return &loadingCommitsRefreshTask{
		refreshRate: refreshRate,
		channels:    channels,
	}
}

func (refreshTask *loadingCommitsRefreshTask) start() {
	log.Debug("Starting commit load refresh task")

	refreshTask.ticker = time.NewTicker(refreshTask.refreshRate)
	cancelCh := make(chan bool)
	refreshTask.cancelCh = cancelCh

	go func(cancelCh <-chan bool) {
		for {
			select {
			case <-refreshTask.ticker.C:
				log.Debug("Updating display with newly loaded commits")
				refreshTask.channels.UpdateDisplay()
			case <-cancelCh:
				refreshTask.channels.UpdateDisplay()
				return
			}
		}
	}(cancelCh)
}

func (refreshTask *loadingCommitsRefreshTask) stop() {
	log.Debug("Stopping commit load refresh task")

	if refreshTask.ticker != nil {
		refreshTask.ticker.Stop()
		refreshTask.cancelCh <- true
		close(refreshTask.cancelCh)
		refreshTask.ticker = nil
	}
}

// OnRefSelect handles a new ref being selected and fetches/loads the relevant commits to display
func (commitView *CommitView) OnRefSelect(ref Ref) (err error) {
	log.Debugf("CommitView loading commits for selected ref %v:%v", ref.Shorthand(), ref.Oid())
	commitView.lock.Lock()
	defer commitView.lock.Unlock()

	if commitView.refreshTask != nil {
		commitView.refreshTask.stop()
	}

	refreshTask := newLoadingCommitsRefreshTask(time.Millisecond*cvLoadRefreshMs, commitView.channels)
	commitView.refreshTask = refreshTask

	if err = commitView.repoData.LoadCommits(ref); err != nil {
		return
	}

	commitView.activeRef = ref

	refViewData, refViewDataExists := commitView.refViewData[ref.Name()]
	if !refViewDataExists {
		refViewData = &referenceViewData{
			viewPos:        NewViewPosition(),
			tableFormatter: NewTableFormatter(cvColumnNum),
		}

		commitView.refViewData[ref.Name()] = refViewData
	}

	commitSetState := commitView.repoData.CommitSetState(ref)

	if commitSetState.loading {
		commitView.refreshTask.start()
		commitView.channels.ReportStatus("Loading commits for ref %v", ref.Shorthand())
	} else {
		commitView.refreshTask.stop()
	}

	var commit *Commit

	if refViewDataExists {
		commitIndex := refViewData.viewPos.ActiveRowIndex()
		commit, err = commitView.repoData.CommitByIndex(commitView.activeRef, commitIndex)
	} else {
		commit, err = commitView.repoData.Commit(commitView.activeRef.Oid())
	}

	if err != nil {
		return
	}

	commitView.notifyCommitViewListeners(commit)

	return
}

// OnCommitsLoaded stops the refresh task if it's still running
func (commitView *CommitView) OnCommitsLoaded(ref Ref) {
	commitView.lock.Lock()
	defer commitView.lock.Unlock()

	if commitView.refreshTask != nil && commitView.activeRef.Name() == ref.Name() {
		log.Debugf("Commits for ref %v loaded. Stopping display refresh task", ref.Name())
		commitView.refreshTask.stop()
	}

	commitSetState := commitView.repoData.CommitSetState(ref)
	commitView.channels.ReportStatus("Loaded %v commits for ref %v", commitSetState.commitNum, ref.Shorthand())
}

// OnCommitsUpdated adjusts the active row index to take account of the newly loaded commits
func (commitView *CommitView) OnCommitsUpdated(ref Ref, updateStartIndex, newCommitNum int) {
	commitView.lock.Lock()
	defer commitView.lock.Unlock()

	if commitView.activeRef.Name() == ref.Name() {
		commitSetState := commitView.repoData.CommitSetState(ref)
		if commitSetState.filterState != nil {
			log.Debugf("Filters applied - leaving active row index unchanged")
			return
		}

		viewPos := commitView.ViewPos()
		rowOffset := newCommitNum - updateStartIndex
		activeRowIndex := MaxInt(int(viewPos.ActiveRowIndex())+rowOffset, 0)

		log.Debugf("Adjusting active row index from %v -> %v for ref %v",
			viewPos.ActiveRowIndex(), activeRowIndex, ref.Name())

		if err := commitView.selectCommit(uint(activeRowIndex)); err != nil {
			commitView.channels.ReportError(err)
		}

		commitView.channels.UpdateDisplay()
	}
}

// OnActiveChange updates whether this view is currently active
func (commitView *CommitView) OnActiveChange(active bool) {
	log.Debugf("CommitView active: %v", active)
	commitView.lock.Lock()
	defer commitView.lock.Unlock()

	commitView.active = active
}

// ViewID returns the ViewID for the commit view
func (commitView *CommitView) ViewID() ViewID {
	return ViewCommit
}

// RegisterCommitViewListener accepts a listener to be notified when a commit is selected
func (commitView *CommitView) RegisterCommitViewListener(commitViewListener CommitViewListener) {
	commitView.lock.Lock()
	defer commitView.lock.Unlock()

	commitView.commitViewListeners = append(commitView.commitViewListeners, commitViewListener)
}

func (commitView *CommitView) notifyCommitViewListeners(commit *Commit) {
	log.Debugf("Notifying commit listeners of selected commit %v", commit.commit.Id().String())

	go func() {
		for _, commitViewListener := range commitView.commitViewListeners {
			if err := commitViewListener.OnCommitSelected(commit); err != nil {
				commitView.channels.ReportError(err)
			}
		}
	}()
}

func (commitView *CommitView) selectCommit(lineIndex uint) (err error) {
	commitIndex := lineIndex
	commitSetState := commitView.repoData.CommitSetState(commitView.activeRef)

	if commitSetState.commitNum == 0 {
		return fmt.Errorf("Cannot select commit as there are no commits for ref %v", commitView.activeRef.Name())
	}

	if commitIndex >= commitSetState.commitNum {
		return fmt.Errorf("Invalid commitIndex: %v, only %v commits are loaded", commitIndex, commitSetState.commitNum)
	}

	selectedCommit, err := commitView.repoData.CommitByIndex(commitView.activeRef, commitIndex)
	if err != nil {
		return
	}

	commitView.ViewPos().SetActiveRowIndex(lineIndex)
	commitView.notifyCommitViewListeners(selectedCommit)

	return
}

func (commitView *CommitView) createCommitViewListenerView(commit *Commit) {
	createViewArgs := CreateViewArgs{
		viewID:   ViewDiff,
		viewArgs: []interface{}{commit.oid.String()},
		registerViewListener: func(observer interface{}) (err error) {
			if observer == nil {
				return fmt.Errorf("Invalid CommitViewListener: %v", observer)
			}

			if commitViewListener, ok := observer.(CommitViewListener); ok {
				commitView.RegisterCommitViewListener(commitViewListener)
			} else {
				err = fmt.Errorf("Observer is not a CommitViewListener but has type %T", observer)
			}

			return
		},
	}

	commitView.channels.DoAction(Action{
		ActionType: ActionSplitView,
		Args: []interface{}{
			ActionSplitViewArgs{
				CreateViewArgs: createViewArgs,
				orientation:    CoDynamic,
			},
		},
	})
}

// ViewPos returns the current view position
func (commitView *CommitView) ViewPos() ViewPos {
	refViewData := commitView.refViewData[commitView.activeRef.Name()]
	return refViewData.viewPos
}

// OnSearchMatch updates the view position when there is a search match
func (commitView *CommitView) OnSearchMatch(startPos ViewPos, matchLineIndex uint) {
	commitView.lock.Lock()
	defer commitView.lock.Unlock()

	viewPos := commitView.ViewPos()

	if viewPos != startPos {
		log.Debugf("Selected ref has changed since search started")
		return
	}

	commitView.selectCommit(matchLineIndex)
}

// Line returns the rendered line at the index provided
func (commitView *CommitView) Line(lineIndex uint) (line string) {
	commitView.lock.Lock()
	defer commitView.lock.Unlock()

	lineNumber := commitView.lineNumber()

	if lineIndex >= lineNumber {
		log.Errorf("Invalid lineIndex: %v", lineIndex)
		return
	}

	refViewData, ok := commitView.refViewData[commitView.activeRef.Name()]
	if !ok {
		log.Errorf("Not refViewData for ref %v", commitView.activeRef.Name())
		return
	}

	tableFormatter := refViewData.tableFormatter
	tableFormatter.Clear()

	commitIndex := lineIndex
	commit, err := commitView.repoData.CommitByIndex(commitView.activeRef, commitIndex)
	if err != nil {
		log.Errorf("Error when retrieving commit during search: %v", err)
		return
	}

	if err = commitView.renderCommit(tableFormatter, 0, commit); err != nil {
		log.Errorf("Error when rendering commit: %v", err)
		return
	}

	line, err = tableFormatter.RowString(0)
	if err != nil {
		log.Errorf("Error when retrieving row string: %v", err)
		return
	}

	return
}

// LineNumber returns the total number of rendered lines the commit view has
func (commitView *CommitView) LineNumber() (lineNumber uint) {
	commitView.lock.Lock()
	defer commitView.lock.Unlock()

	return commitView.lineNumber()
}

func (commitView *CommitView) lineNumber() (lineNumber uint) {
	commitSetState := commitView.repoData.CommitSetState(commitView.activeRef)
	lineNum := commitSetState.commitNum

	return lineNum
}

// HandleEvent does nothing
func (commitView *CommitView) HandleEvent(event Event) (err error) {
	return
}

// HandleAction checks if commit view supports this action and if it does executes it
func (commitView *CommitView) HandleAction(action Action) (err error) {
	log.Debugf("CommitView handling action %v", action)
	commitView.lock.Lock()
	defer commitView.lock.Unlock()

	if commitView.activeRef == nil {
		return
	}

	if handler, ok := commitView.handlers[action.ActionType]; ok {
		err = handler(commitView, action)
	} else {
		_, err = commitView.viewSearch.HandleAction(action)
	}

	return
}

func moveUpCommit(commitView *CommitView, action Action) (err error) {
	viewPos := commitView.ViewPos()

	if viewPos.MoveLineUp() {
		log.Debug("Moving up one commit")
		if err = commitView.selectCommit(viewPos.ActiveRowIndex()); err != nil {
			return
		}
		commitView.channels.UpdateDisplay()
	}

	return
}

func moveDownCommit(commitView *CommitView, action Action) (err error) {
	lineNumber := commitView.lineNumber()
	viewPos := commitView.ViewPos()

	if viewPos.MoveLineDown(lineNumber) {
		log.Debug("Moving down one commit")
		if err = commitView.selectCommit(viewPos.ActiveRowIndex()); err != nil {
			return
		}
		commitView.channels.UpdateDisplay()
	}

	return
}

func moveUpCommitPage(commitView *CommitView, action Action) (err error) {
	viewPos := commitView.ViewPos()

	if viewPos.MovePageUp(commitView.viewDimension.rows - 2) {
		log.Debug("Moving up one page")
		if err = commitView.selectCommit(viewPos.ActiveRowIndex()); err != nil {
			return
		}
		commitView.channels.UpdateDisplay()
	}

	return
}

func moveDownCommitPage(commitView *CommitView, action Action) (err error) {
	lineNumber := commitView.lineNumber()
	viewPos := commitView.ViewPos()

	if viewPos.MovePageDown(commitView.viewDimension.rows-2, lineNumber) {
		log.Debug("Moving down one page")
		if err = commitView.selectCommit(viewPos.ActiveRowIndex()); err != nil {
			return
		}
		commitView.channels.UpdateDisplay()
	}

	return
}

func scrollCommitViewRight(commitView *CommitView, action Action) (err error) {
	viewPos := commitView.ViewPos()
	viewPos.MovePageRight(commitView.viewDimension.cols)
	log.Debugf("Scrolling right. View starts at column %v", viewPos.ViewStartColumn())
	commitView.channels.UpdateDisplay()

	return
}

func scrollCommitViewLeft(commitView *CommitView, action Action) (err error) {
	viewPos := commitView.ViewPos()

	if viewPos.MovePageLeft(commitView.viewDimension.cols) {
		log.Debugf("Scrolling left. View starts at column %v", viewPos.ViewStartColumn())
		commitView.channels.UpdateDisplay()
	}

	return
}

func moveToFirstCommit(commitView *CommitView, action Action) (err error) {
	viewPos := commitView.ViewPos()

	if viewPos.MoveToFirstLine() {
		log.Debug("Moving up to first commit")
		if err = commitView.selectCommit(viewPos.ActiveRowIndex()); err != nil {
			return
		}
		commitView.channels.UpdateDisplay()
	}

	return
}

func moveToLastCommit(commitView *CommitView, action Action) (err error) {
	lineNumber := commitView.lineNumber()
	viewPos := commitView.ViewPos()

	if viewPos.MoveToLastLine(lineNumber) {
		log.Debug("Moving to last commit")
		if err = commitView.selectCommit(viewPos.ActiveRowIndex()); err != nil {
			return
		}
		commitView.channels.UpdateDisplay()
	}

	return
}

func addCommitFilter(commitView *CommitView, action Action) (err error) {
	if !(len(action.Args) > 0) {
		return fmt.Errorf("Expected filter query argument")
	}

	query, ok := action.Args[0].(string)
	if !ok {
		return fmt.Errorf("Expected filter query argument to have type string")
	}

	commitFilter, errors := CreateCommitFilter(query)
	if len(errors) > 0 {
		commitView.channels.ReportErrors(errors)
		return
	}

	if err = commitView.repoData.AddCommitFilter(commitView.activeRef, commitFilter); err != nil {
		return
	}

	commitView.ViewPos().SetActiveRowIndex(0)

	go func() {
		// TODO: Works in practice, but there is no guarantee the filtered commit set will have
		// been populated after 250ms. Need an event based mechanism to be notified when a filtered
		// set has started to be populated so that the first commit can be selected
		time.Sleep(250 * time.Millisecond)
		commitView.lock.Lock()
		defer commitView.lock.Unlock()

		if err := commitView.selectCommit(commitView.ViewPos().ActiveRowIndex()); err != nil {
			log.Errorf("Unable to select commit after filter has been applied: %v", err)
		}
	}()

	commitView.channels.UpdateDisplay()

	return
}

func removeCommitFilter(commitView *CommitView, action Action) (err error) {
	if err = commitView.repoData.RemoveCommitFilter(commitView.activeRef); err != nil {
		return
	}

	if err = commitView.selectCommit(0); err != nil {
		return
	}

	commitView.channels.UpdateDisplay()

	return
}

func centerCommitView(commitView *CommitView, action Action) (err error) {
	viewPos := commitView.ViewPos()

	if viewPos.CenterActiveRow(commitView.viewDimension.rows - 2) {
		log.Debug("Centering CommitView")
		commitView.channels.UpdateDisplay()
	}

	return
}

func selectCommit(commitView *CommitView, action Action) (err error) {
	viewPos := commitView.ViewPos()

	if len(commitView.commitViewListeners) == 0 {
		var commit *Commit
		if commit, err = commitView.repoData.CommitByIndex(commitView.activeRef, viewPos.ActiveRowIndex()); err != nil {
			return
		}

		commitView.createCommitViewListenerView(commit)
	}

	return commitView.selectCommit(viewPos.ActiveRowIndex())
}
