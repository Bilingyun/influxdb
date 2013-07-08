package raft

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io/ioutil"
	"os"
	"path"
	"sort"
	"sync"
	"time"
)

//------------------------------------------------------------------------------
//
// Constants
//
//------------------------------------------------------------------------------

const (
	Stopped   = "stopped"
	Follower  = "follower"
	Candidate = "candidate"
	Leader    = "leader"
)

const (
	DefaultHeartbeatTimeout = 50 * time.Millisecond
	DefaultElectionTimeout  = 150 * time.Millisecond
)

var stopValue interface{}

//------------------------------------------------------------------------------
//
// Errors
//
//------------------------------------------------------------------------------

var NotLeaderError = errors.New("raft.Server: Not current leader")
var DuplicatePeerError = errors.New("raft.Server: Duplicate peer")
var CommandTimeoutError = errors.New("raft: Command timeout")

//------------------------------------------------------------------------------
//
// Typedefs
//
//------------------------------------------------------------------------------

// A server is involved in the consensus protocol and can act as a follower,
// candidate or a leader.
type Server struct {
	name        string
	path        string
	state       string
	transporter Transporter
	context     interface{}
	currentTerm uint64

	votedFor    string
	log         *Log
	leader      string
	peers       map[string]*Peer
	mutex       sync.RWMutex
	commitCount int

	c                chan *event
	electionTimeout  time.Duration
	heartbeatTimeout time.Duration

	currentSnapshot *Snapshot
	lastSnapshot    *Snapshot
	stateMachine    StateMachine
}

// An event to be processed by the server's event loop.
type event struct {
	target      interface{}
	returnValue interface{}
	c           chan error
}

//------------------------------------------------------------------------------
//
// Constructor
//
//------------------------------------------------------------------------------

// Creates a new server with a log at the given path.
func NewServer(name string, path string, transporter Transporter, stateMachine StateMachine, context interface{}) (*Server, error) {
	if name == "" {
		return nil, errors.New("raft.Server: Name cannot be blank")
	}
	if transporter == nil {
		panic("raft: Transporter required")
	}

	s := &Server{
		name:             name,
		path:             path,
		transporter:      transporter,
		stateMachine:     stateMachine,
		context:          context,
		state:            Stopped,
		peers:            make(map[string]*Peer),
		log:              newLog(),
		c:                make(chan *event, 256),
		electionTimeout:  DefaultElectionTimeout,
		heartbeatTimeout: DefaultHeartbeatTimeout,
	}

	// Setup apply function.
	s.log.ApplyFunc = func(c Command) (interface{}, error) {
		result, err := c.Apply(s)
		return result, err
	}

	return s, nil
}

//------------------------------------------------------------------------------
//
// Accessors
//
//------------------------------------------------------------------------------

//--------------------------------------
// General
//--------------------------------------

// Retrieves the name of the server.
func (s *Server) Name() string {
	return s.name
}

// Retrieves the storage path for the server.
func (s *Server) Path() string {
	return s.path
}

// The name of the current leader.
func (s *Server) Leader() string {
	return s.leader
}

// Retrieves a copy of the peer data.
func (s *Server) Peers() map[string]*Peer {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	peers := make(map[string]*Peer)
	for name, peer := range s.peers {
		peers[name] = peer.clone()
	}
	return peers
}

// Retrieves the object that transports requests.
func (s *Server) Transporter() Transporter {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return s.transporter
}

func (s *Server) SetTransporter(t Transporter) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.transporter = t
}

// Retrieves the context passed into the constructor.
func (s *Server) Context() interface{} {
	return s.context
}

// Retrieves the log path for the server.
func (s *Server) LogPath() string {
	return fmt.Sprintf("%s/log", s.path)
}

// Retrieves the current state of the server.
func (s *Server) State() string {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return s.state
}

// Sets the state of the server.
func (s *Server) setState(state string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.state = state
	if state == Leader {
		s.leader = s.Name()
	}
}

// Retrieves the current term of the server.
func (s *Server) Term() uint64 {
	return s.currentTerm
}

// Retrieves the current commit index of the server.
func (s *Server) CommitIndex() uint64 {
	return s.log.commitIndex
}

// Retrieves the name of the candidate this server voted for in this term.
func (s *Server) VotedFor() string {
	return s.votedFor
}

// Retrieves whether the server's log has no entries.
func (s *Server) IsLogEmpty() bool {
	return s.log.isEmpty()
}

// A list of all the log entries. This should only be used for debugging purposes.
func (s *Server) LogEntries() []*LogEntry {
	return s.log.entries
}

// A reference to the command name of the last entry.
func (s *Server) LastCommandName() string {
	return s.log.lastCommandName()
}

// Get the state of the server for debugging
func (s *Server) GetState() string {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return fmt.Sprintf("Name: %s, State: %s, Term: %v, Index: %v ", s.name, s.state, s.currentTerm, s.log.commitIndex)
}

//--------------------------------------
// Membership
//--------------------------------------

// Retrieves the number of member servers in the consensus.
func (s *Server) MemberCount() int {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return len(s.peers) + 1
}

// Retrieves the number of servers required to make a quorum.
func (s *Server) QuorumSize() int {
	return (s.MemberCount() / 2) + 1
}

//--------------------------------------
// Election timeout
//--------------------------------------

// Retrieves the election timeout.
func (s *Server) ElectionTimeout() time.Duration {
	return s.electionTimeout
}

// Sets the election timeout.
func (s *Server) SetElectionTimeout(duration time.Duration) {
	s.electionTimeout = duration
}

//--------------------------------------
// Heartbeat timeout
//--------------------------------------

// Retrieves the heartbeat timeout.
func (s *Server) HeartbeatTimeout() time.Duration {
	return s.heartbeatTimeout
}

// Sets the heartbeat timeout.
func (s *Server) SetHeartbeatTimeout(duration time.Duration) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.heartbeatTimeout = duration
	for _, peer := range s.peers {
		peer.setHeartbeatTimeout(duration)
	}
}

//------------------------------------------------------------------------------
//
// Methods
//
//------------------------------------------------------------------------------

//--------------------------------------
// Initialization
//--------------------------------------

// Starts the server with a log at the given path.
func (s *Server) Initialize() error {

	// Exit if the server is already running.
	if s.state != Stopped {
		return errors.New("raft.Server: Server already running")
	}

	// Create snapshot directory if not exist
	os.Mkdir(s.path+"/snapshot", 0700)

	// Initialize the log and load it up.
	if err := s.log.open(s.LogPath()); err != nil {
		s.debugln("raft: Log error: %s", err)
		return fmt.Errorf("raft: Initialization error: %s", err)
	}

	// Update the term to the last term in the log.
	s.currentTerm = s.log.currentTerm()

	return nil
}

// Start the sever as a follower
func (s *Server) StartFollower() {
	s.setState(Follower)
	go s.loop()
}

// Start the sever as a leader
func (s *Server) StartLeader() {
	s.setState(Leader)
	s.currentTerm++
	go s.loop()
}

// Shuts down the server.
func (s *Server) Stop() {
	s.send(&stopValue)
	s.mutex.Lock()
	s.log.close()
	s.mutex.Unlock()
}

// Checks if the server is currently running.
func (s *Server) Running() bool {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return s.state != Stopped
}

//--------------------------------------
// Term
//--------------------------------------

// Sets the current term for the server. This is only used when an external
// current term is found.
func (s *Server) setCurrentTerm(term uint64, leaderName string, append bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	// update the term and clear vote for
	if term > s.currentTerm {
		s.state = Follower
		s.currentTerm = term
		s.leader = leaderName
		s.votedFor = ""
		return
	}

	// discover new leader
	if term == s.currentTerm && s.state == Candidate && append {
		s.state = Follower
		s.leader = leaderName
	}
}

//--------------------------------------
// Event Loop
//--------------------------------------

//                          timeout
//                          ______
//                         |      |
//                         |      |
//                         v      |     recv majority votes
//  --------    timeout    -----------                        -----------
// |Follower| ----------> | Candidate |--------------------> |  Leader   |
//  --------               -----------                        -----------
//     ^          higher term/ |                         higher term |
//     |            new leader |                                     |
//     |_______________________|____________________________________ |

// The main event loop for the server
func (s *Server) loop() {
	defer s.debugln("server.loop.end")

	for {
		state := s.State()

		s.debugln("server.loop.run ", state)
		switch state {
		case Follower:
			s.followerLoop()

		case Candidate:
			s.candidateLoop()

		case Leader:
			s.leaderLoop()

		case Stopped:
			return
		}
	}
}

// Sends an event to the event loop to be processed. The function will wait
// until the event is actually processed before returning.
func (s *Server) send(value interface{}) (interface{}, error) {
	event := s.sendAsync(value)
	err := <-event.c
	return event.returnValue, err
}

func (s *Server) sendAsync(value interface{}) *event {
	event := &event{target: value, c: make(chan error, 1)}
	s.c <- event
	return event
}

// The event loop that is run when the server is in a Follower state.
// Responds to RPCs from candidates and leaders.
// Converts to candidate if election timeout elapses without either:
//   1.Receiving valid AppendEntries RPC, or
//   2.Granting vote to candidate
func (s *Server) followerLoop() {

	s.setState(Follower)
	timeoutChan := afterBetween(s.ElectionTimeout(), s.ElectionTimeout()*2)

	for {
		var err error
		var update bool
		select {
		case e := <-s.c:
			if e.target == &stopValue {
				s.setState(Stopped)
			} else if _, ok := e.target.(Command); ok {
				err = NotLeaderError
			} else if req, ok := e.target.(*AppendEntriesRequest); ok {
				e.returnValue, update = s.processAppendEntriesRequest(req)
			} else if req, ok := e.target.(*RequestVoteRequest); ok {
				e.returnValue, update = s.processRequestVoteRequest(req)
			}

			// Callback to event.
			e.c <- err

		case <-timeoutChan:
			s.setState(Candidate)
		}

		// Converts to candidate if election timeout elapses without either:
		//   1.Receiving valid AppendEntries RPC, or
		//   2.Granting vote to candidate
		if update {
    		timeoutChan = afterBetween(s.ElectionTimeout(), s.ElectionTimeout()*2)
		}

		// Exit loop on state change.
		if s.State() != Follower {
			break
		}
	}
}

// The event loop that is run when the server is in a Candidate state.
func (s *Server) candidateLoop() {
	lastLogIndex, lastLogTerm := s.log.lastInfo()
	s.leader = ""

	for {
		// Increment current term, vote for self.
		s.currentTerm++
		s.votedFor = s.name

		// Send RequestVote RPCs to all other servers.
		respChan := make(chan *RequestVoteResponse, len(s.peers))
		for _, peer := range s.peers {
			go peer.sendVoteRequest(newRequestVoteRequest(s.currentTerm, s.name, lastLogIndex, lastLogTerm), respChan)
		}

		// Wait for either:
		//   * Votes received from majority of servers: become leader
		//   * AppendEntries RPC received from new leader: step down.
		//   * Election timeout elapses without election resolution: increment term, start new election
		//   * Discover higher term: step down (§5.1)
		votesGranted := 1
		timeoutChan := afterBetween(s.ElectionTimeout(), s.ElectionTimeout()*2)
		timeout := false

		for {
			// If we received enough votes then stop waiting for more votes.
			s.debugln("server.candidate.votes: ", votesGranted, " quorum:", s.QuorumSize())
			if votesGranted >= s.QuorumSize() {
				s.setState(Leader)
				break
			}

			// Collect votes from peers.
			select {
			case resp := <-respChan:
				if resp.VoteGranted {
					s.debugln("server.candidate.vote.granted: ", votesGranted)
					votesGranted++
				} else if resp.Term > s.currentTerm {
					s.debugln("server.candidate.vote.failed")
					s.setCurrentTerm(resp.Term, "", false)
				}

			case e := <-s.c:
				var err error
				if e.target == &stopValue {
					s.setState(Stopped)
					break
				} else if _, ok := e.target.(Command); ok {
					err = NotLeaderError
				} else if req, ok := e.target.(*AppendEntriesRequest); ok {
					e.returnValue, _ = s.processAppendEntriesRequest(req)
				} else if req, ok := e.target.(*RequestVoteRequest); ok {
					e.returnValue, _ = s.processRequestVoteRequest(req)
				}

				// Callback to event.
				e.c <- err

			case <-timeoutChan:
				timeout = true
			}

			// both process AER and RVR can make the server to follower
			// also break when timeout happens
			if s.State() == Follower || timeout {
				break
			}
		}

		// break when we are not candidate
		if s.State() != Candidate {
			break
		}

		// continue when timeout happened
	}
}

// The event loop that is run when the server is in a Candidate state.
func (s *Server) leaderLoop() {
	s.setState(Leader)
	s.commitCount = 0
	logIndex, _ := s.log.lastInfo()

	// Update the peers prevLogIndex to leader's lastLogIndex and start heartbeat.
	for _, peer := range s.peers {
		peer.setPrevLogIndex(logIndex)
		peer.startHeartbeat()
	}

	// Begin to collect response from followers
	for {
		var err error
		select {
		case e := <-s.c:
			s.debugln("server.leader.select")

			if e.target == &stopValue {
				s.setState(Stopped)
			} else if command, ok := e.target.(Command); ok {
				s.processCommand(command, e)
				continue
			} else if req, ok := e.target.(*AppendEntriesRequest); ok {
				e.returnValue, _ = s.processAppendEntriesRequest(req)
			} else if resp, ok := e.target.(*AppendEntriesResponse); ok {
				s.processAppendEntriesResponse(resp)
			} else if req, ok := e.target.(*RequestVoteRequest); ok {
				e.returnValue, _ = s.processRequestVoteRequest(req)
			}

			// Callback to event.
			e.c <- err
		}

		// Exit loop on state change.
		if s.State() != Leader {
			break
		}
	}

	// Stop all peers.
	for _, peer := range s.peers {
		peer.stopHeartbeat()
	}
}

//--------------------------------------
// Commands
//--------------------------------------

// Attempts to execute a command and replicate it. The function will return
// when the command has been successfully committed or an error has occurred.

func (s *Server) Do(command Command) (interface{}, error) {
	return s.send(command)
}

// Processes a command.
func (s *Server) processCommand(command Command, e *event) {
	s.debugln("server.command.process")

	// Create an entry for the command in the log.
	entry := s.log.createEntry(s.currentTerm, command)
	if err := s.log.appendEntry(entry); err != nil {
		s.debugln("server.command.log.error:", err)
		e.c <- err
		return
	}

	// Issue a callback for the entry once it's committed.
	go func() {
		// Wait for the entry to be committed.
		select {
		case <-entry.commit:
			var err error
			s.debugln("server.command.commit")
			e.returnValue, err = s.log.getEntryResult(entry, true)
			e.c <- err
		case <-time.After(time.Second):
			s.debugln("server.command.timeout")
			e.c <- CommandTimeoutError
		}
	}()

	// Issue an append entries response for the server.
	s.sendAsync(newAppendEntriesResponse(s.currentTerm, true, s.log.CommitIndex()))
}

//--------------------------------------
// Append Entries
//--------------------------------------

// Appends zero or more log entry from the leader to this server.
func (s *Server) AppendEntries(req *AppendEntriesRequest) *AppendEntriesResponse {
	ret, _ := s.send(req)
	resp, _ := ret.(*AppendEntriesResponse)
	return resp
}

// Processes the "append entries" request.
func (s *Server) processAppendEntriesRequest(req *AppendEntriesRequest) (*AppendEntriesResponse, bool) {
	if req.Term < s.currentTerm {
		s.debugln("server.ae.error: stale term")
		return newAppendEntriesResponse(s.currentTerm, false, s.log.CommitIndex()), false
	}

	// Update term and leader.
	s.setCurrentTerm(req.Term, req.LeaderName, true)

	// Reject if log doesn't contain a matching previous entry.
	if err := s.log.truncate(req.PrevLogIndex, req.PrevLogTerm); err != nil {
		s.debugln("server.ae.truncate.error: ", err)
		return newAppendEntriesResponse(s.currentTerm, false, s.log.CommitIndex()), true
	}

	// Append entries to the log.
	if err := s.log.appendEntries(req.Entries); err != nil {
		s.debugln("server.ae.append.error: ", err)
		return newAppendEntriesResponse(s.currentTerm, false, s.log.CommitIndex()), true
	}

	// Commit up to the commit index.
	if err := s.log.setCommitIndex(req.CommitIndex); err != nil {
		s.debugln("server.ae.commit.error: ", err)
		return newAppendEntriesResponse(s.currentTerm, false, s.log.CommitIndex()), true
	}

	return newAppendEntriesResponse(s.currentTerm, true, s.log.CommitIndex()), true
}

// Processes the "append entries" response from the peer. This is only
// processed when the server is a leader. Responses received during other
// states are dropped.
func (s *Server) processAppendEntriesResponse(resp *AppendEntriesResponse) {
	// If we find a higher term then change to a follower and exit.
	if resp.Term > s.currentTerm {
		s.setCurrentTerm(resp.Term, "", false)
		return
	}

	// Ignore response if it's not successful.
	if !resp.Success {
		return
	}

	// Increment the commit count to make sure we have a quorum before committing.
	s.commitCount++
	if s.commitCount < s.QuorumSize() {
		return
	}

	// Determine the committed index that a majority has.
	var indices []uint64
	indices = append(indices, s.log.currentIndex())
	for _, peer := range s.peers {
		indices = append(indices, peer.getPrevLogIndex())
	}
	sort.Sort(uint64Slice(indices))

	// We can commit up to the index which the majority of the members have appended.
	commitIndex := indices[s.QuorumSize()-1]
	committedIndex := s.log.commitIndex

	if commitIndex > committedIndex {
		s.log.setCommitIndex(commitIndex)
		s.debugln("commit index ", commitIndex)
		for i := committedIndex; i < commitIndex; i++ {
			if entry := s.log.getEntry(i + 1); entry != nil {
				// if the leader is a new one and the entry came from the 
				// old leader, the commit channel will be nil and no go routine
				// is waiting from this channel
				// if we try to send to it, the new leader will get stuck
				if entry.commit != nil {
					select {
					case entry.commit <- true:
					default:
						panic("server unable to send signal to commit channel")
					}
				}
			}
		}
	}
}

//--------------------------------------
// Request Vote
//--------------------------------------

// Requests a vote from a server. A vote can be obtained if the vote's term is
// at the server's current term and the server has not made a vote yet. A vote
// can also be obtained if the term is greater than the server's current term.
func (s *Server) RequestVote(req *RequestVoteRequest) *RequestVoteResponse {
	ret, _ := s.send(req)
	resp, _ := ret.(*RequestVoteResponse)
	return resp
}

// Processes a "request vote" request.
func (s *Server) processRequestVoteRequest(req *RequestVoteRequest) (*RequestVoteResponse, bool) {
	// If the request is coming from an old term then reject it.
	if req.Term < s.currentTerm {
		s.debugln("server.rv.error: stale term")
		return newRequestVoteResponse(s.currentTerm, false), false
	}

	s.setCurrentTerm(req.Term, "", false)

	// If we've already voted for a different candidate then don't vote for this candidate.
	if s.votedFor != "" && s.votedFor != req.CandidateName {
		s.debugln("server.rv.error: duplicate vote: ", req.CandidateName,
			" already vote for ", s.votedFor)
		return newRequestVoteResponse(s.currentTerm, false), false
	}

	// If the candidate's log is not at least as up-to-date as our last log then don't vote.
	lastIndex, lastTerm := s.log.lastInfo()
	if lastIndex > req.LastLogIndex || lastTerm > req.LastLogTerm {
		s.debugln("server.rv.error: out of date log: ", req.CandidateName,
			"[", lastIndex, "]", " [", req.LastLogIndex, "]")
		return newRequestVoteResponse(s.currentTerm, false), false
	}

	// If we made it this far then cast a vote and reset our election time out.
	s.debugln("server.rv.vote: ", s.name, " votes for", req.CandidateName, "at term", req.Term)
	s.votedFor = req.CandidateName

	return newRequestVoteResponse(s.currentTerm, true), true
}

//--------------------------------------
// Membership
//--------------------------------------

// Adds a peer to the server. This should be called by a system's join command
// within the context so that it is within the context of the server lock.
func (s *Server) AddPeer(name string) error {
	// Do not allow peers to be added twice.

	if s.peers[name] != nil {
		return DuplicatePeerError
	}

	// Only add the peer if it doesn't have the same name.
	if s.name != name {
		//s.debugln("Add peer ", name)
		peer := newPeer(s, name, s.heartbeatTimeout)
		if s.State() == Leader {
			peer.startHeartbeat()
		}
		s.peers[peer.name] = peer
	}

	return nil
}

// Removes a peer from the server. This should be called by a system's join command
// within the context so that it is within the context of the server lock.
func (s *Server) RemovePeer(name string) error {
	// Ignore removal of the server itself.
	if s.name == name {
		return nil
	}
	// Return error if peer doesn't exist.
	peer := s.peers[name]
	if peer == nil {
		return fmt.Errorf("raft: Peer not found: %s", name)
	}

	// TODO: Flush entries to the peer first.

	// Stop peer and remove it.
	peer.stopHeartbeat()
	delete(s.peers, name)

	return nil
}

//--------------------------------------
// Log compaction
//--------------------------------------

// The background snapshot function
func (s *Server) Snapshot() {
	for {
		// TODO: change this... to something reasonable
		time.Sleep(60 * time.Second)

		s.takeSnapshot()
	}
}

func (s *Server) takeSnapshot() error {
	//TODO put a snapshot mutex
	s.debugln("take Snapshot")
	if s.currentSnapshot != nil {
		return errors.New("handling snapshot")
	}

	lastIndex, lastTerm := s.log.commitInfo()

	if lastIndex == 0 || lastTerm == 0 {
		return errors.New("No logs")
	}

	path := s.SnapshotPath(lastIndex, lastTerm)

	var state []byte
	var err error

	if s.stateMachine != nil {
		state, err = s.stateMachine.Save()

		if err != nil {
			return err
		}

	} else {
		state = []byte{0}
	}

	var peerNames []string

	for _, peer := range s.peers {
		peerNames = append(peerNames, peer.Name())
	}
	peerNames = append(peerNames, s.Name())

	s.currentSnapshot = &Snapshot{lastIndex, lastTerm, peerNames, state, path}

	s.saveSnapshot()

	s.log.compact(lastIndex, lastTerm)

	return nil
}

// Retrieves the log path for the server.
func (s *Server) saveSnapshot() error {

	if s.currentSnapshot == nil {
		return errors.New("no snapshot to save")
	}

	err := s.currentSnapshot.save()

	if err != nil {
		return err
	}

	tmp := s.lastSnapshot
	s.lastSnapshot = s.currentSnapshot

	// delete the previous snapshot if there is any change
	if tmp != nil && !(tmp.LastIndex == s.lastSnapshot.LastIndex && tmp.LastTerm == s.lastSnapshot.LastTerm) {
		tmp.remove()
	}
	s.currentSnapshot = nil
	return nil
}

// Retrieves the log path for the server.
func (s *Server) SnapshotPath(lastIndex uint64, lastTerm uint64) string {
	return path.Join(s.path, "snapshot", fmt.Sprintf("%v_%v.ss", lastTerm, lastIndex))
}

func (s *Server) SnapshotRecovery(req *SnapshotRequest) (*SnapshotResponse, error) {
	//
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.stateMachine.Recovery(req.State)

	//recovery the cluster configuration
	for _, peerName := range req.Peers {
		s.AddPeer(peerName)
	}

	//update term and index
	s.currentTerm = req.LastTerm

	s.log.updateCommitIndex(req.LastIndex)

	snapshotPath := s.SnapshotPath(req.LastIndex, req.LastTerm)

	s.currentSnapshot = &Snapshot{req.LastIndex, req.LastTerm, req.Peers, req.State, snapshotPath}

	s.saveSnapshot()

	s.log.compact(req.LastIndex, req.LastTerm)

	return newSnapshotResponse(req.LastTerm, true, req.LastIndex), nil

}

// Load a snapshot at restart
func (s *Server) LoadSnapshot() error {
	dir, err := os.OpenFile(path.Join(s.path, "snapshot"), os.O_RDONLY, 0)
	if err != nil {

		return err
	}

	filenames, err := dir.Readdirnames(-1)

	if err != nil {
		dir.Close()
		panic(err)
	}

	dir.Close()
	if len(filenames) == 0 {
		return errors.New("no snapshot")
	}

	// not sure how many snapshot we should keep
	sort.Strings(filenames)
	snapshotPath := path.Join(s.path, "snapshot", filenames[len(filenames)-1])

	// should not fail
	file, err := os.OpenFile(snapshotPath, os.O_RDONLY, 0)
	defer file.Close()
	if err != nil {
		panic(err)
	}

	// TODO check checksum first

	var snapshotBytes []byte
	var checksum uint32

	n, err := fmt.Fscanf(file, "%08x\n", &checksum)

	if err != nil {
		return err
	}

	if n != 1 {
		return errors.New("Bad snapshot file")
	}

	snapshotBytes, _ = ioutil.ReadAll(file)
	s.debugln(string(snapshotBytes))

	// Generate checksum.
	byteChecksum := crc32.ChecksumIEEE(snapshotBytes)

	if uint32(checksum) != byteChecksum {
		s.debugln(checksum, " ", byteChecksum)
		return errors.New("bad snapshot file")
	}

	err = json.Unmarshal(snapshotBytes, &s.lastSnapshot)

	if err != nil {
		s.debugln("unmarshal error: ", err)
		return err
	}

	err = s.stateMachine.Recovery(s.lastSnapshot.State)

	if err != nil {
		s.debugln("recovery error: ", err)
		return err
	}

	for _, peerName := range s.lastSnapshot.Peers {
		s.AddPeer(peerName)
	}

	s.log.startTerm = s.lastSnapshot.LastTerm
	s.log.startIndex = s.lastSnapshot.LastIndex
	s.log.updateCommitIndex(s.lastSnapshot.LastIndex)

	return err
}

//--------------------------------------
// Debugging
//--------------------------------------

func (s *Server) debugln(v ...interface{}) {
	debugf("[%s] %s", s.name, fmt.Sprintln(v...))
}

func (s *Server) traceln(v ...interface{}) {
	tracef("[%s] %s", s.name, fmt.Sprintln(v...))
}
