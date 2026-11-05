------------------------------ MODULE dbfirst ------------------------------
(***************************************************************************)
(* The write order of one repository on one gitai node, in authoritative   *)
(* storage mode, with and without DB-first writes                          *)
(* (doc/graft/design/dbfirst-writes.md).                                   *)
(*                                                                         *)
(* What is modelled:                                                       *)
(*  - the database: one dense, ordered changeset log per repository (log), *)
(*    committed atomically under the repository row lock, with a           *)
(*    per-reference compare-and-swap (GuardCAS) and land identities;       *)
(*  - the node's local git copy (local), its watermark (wm) and the        *)
(*    sequences NoteCommitted remembers (notes, volatile);                 *)
(*  - git's repository-wide reference lock (tables.list.lock plus the      *)
(*    FIFO RefAdmission in front of it) as one mutex (lock);               *)
(*  - writers of three kinds:                                              *)
(*      "rpc"    an OperationService writer (UpdaterWithHooks). With       *)
(*               DBFirst it commits to the database without git's lock and *)
(*               the applier brings the local copy up; without DBFirst it  *)
(*               runs the current hook path (lock, git check, voter commit *)
(*               in Prepared, git write).                                  *)
(*      "push"   a non-atomic receive-pack update: always the hook path.   *)
(*               git still feeds an update it rejected to the hook         *)
(*               (refs.c transaction_hook_feed_stdin).                     *)
(*      "remote" a writer on another node: a database commit only.         *)
(*  - the request gate (every request waits for wm >= head at arrival);    *)
(*  - the applier (the syncer's catch-up): applies a prefix of the log      *)
(*    under git's lock, never reverting a newer local write                *)
(*    (GuardNeverRevert);                                                  *)
(*  - a process crash (git's lock and the volatile notes are lost, local   *)
(*    references survive in the page cache) and a power loss (the local    *)
(*    copy is moved aside and hydrated at the database head).              *)
(*                                                                         *)
(* What is abstracted: reference names (D/F conflicts, HEAD, symbolic      *)
(* references; the database's refuseRefNameConflicts is the only name     *)
(* check that matters for safety and it runs in the commit), objects and   *)
(* packs, policy (a deterministic check before the commit), reftable       *)
(* compaction and fsync. A value is the writer that wrote it, so values    *)
(* are unique and a value names its changeset. Every writer commits at     *)
(* most once; a client's retry is another writer with the same land.      *)
(***************************************************************************)
EXTENDS Naturals, Sequences, FiniteSets, TLC

CONSTANTS
    Branches,          \* reference names, e.g. {"b1", "b2"}
    Writers,           \* writer ids, e.g. {"w1", "w2", "w3", "w4"}
    Kind,              \* [Writers -> {"rpc", "push", "remote"}]
    Target,            \* [Writers -> Branches]
    Land,              \* [Writers -> land ids \cup {NoLand}]
    Init0,             \* [Branches -> {"init", "none"}]; "none" is absent
    DBFirst,           \* rpc writers take the DB-first path
    GuardCAS,          \* the database commit checks the expected old value
    GuardNeverRevert,  \* the applier never moves a reference back
    GuardStaleCreate,  \* the 02fbb52 stop-gap: zero old, local ref exists
    GuardStaleUpdate,  \* proposed: local value under the lock /= old value
    GuardDBBeforeLocal,\* git writes the local reference after the DB commit
    MaxCrashes

NoLand == "noland"
Values == Writers \cup {"init", "none"}
Terminal == {"acked", "rejected", "unknown", "failed"}
PCs == {"idle", "arriving", "read", "committed", "locked", "prepared",
        "gitRejected", "dbdone", "localfirst"} \cup Terminal

VARIABLES
    log,      \* Seq of [seq, ref, old, new, exp, land, w]
    local,    \* [Branches -> Values]: the local git references
    wm,       \* the watermark: changesets 1..wm are applied locally
    notes,    \* sequences committed locally by the hook path (volatile)
    lock,     \* "free" or the holder of git's reference lock
    pc,       \* [Writers -> PCs]
    old,      \* [Writers -> Values]: the expected old value of the writer
    h0,       \* [Writers -> Nat]: database head when the request arrived
    crashes

vars == <<log, local, wm, notes, lock, pc, old, h0, crashes>>

-----------------------------------------------------------------------------
(* Database views *)

Max(S) == CHOOSE x \in S : \A y \in S : y <= x

LastWrite(b, s) ==
    LET I == {i \in 1..s : log[i].ref = b} IN IF I = {} THEN 0 ELSE Max(I)

ValueAt(b, s) ==
    LET j == LastWrite(b, s) IN IF j = 0 THEN Init0[b] ELSE log[j].new

DB(b) == ValueAt(b, Len(log))

\* The changeset that produced v; 0 for the initial values.
SeqOfValue(v) ==
    IF \E i \in 1..Len(log) : log[i].new = v
    THEN CHOOSE i \in 1..Len(log) : log[i].new = v
    ELSE 0

Committed(w) == \E i \in 1..Len(log) : log[i].w = w
SeqOfW(w) == SeqOfValue(w)

DBLanded == {log[i].land : i \in 1..Len(log)} \ {NoLand}

\* The lands in the history of value v (git's ancestry of a branch tip).
RECURSIVE Contains(_)
Contains(v) ==
    IF SeqOfValue(v) = 0 THEN {}
    ELSE LET i == SeqOfValue(v)
         IN Contains(log[i].old) \cup ({log[i].land} \ {NoLand})

\* Append the changeset of w; exp is what the writer expected ("any" = forced).
CommitOf(w, expv) ==
    log' = Append(log, [seq  |-> Len(log) + 1,
                        ref  |-> Target[w],
                        old  |-> DB(Target[w]),
                        new  |-> w,
                        exp  |-> expv,
                        land |-> Land[w],
                        w    |-> w])

-----------------------------------------------------------------------------
Init ==
    /\ log = <<>>
    /\ local = Init0
    /\ wm = 0
    /\ notes = {}
    /\ lock = "free"
    /\ pc = [w \in Writers |-> "idle"]
    /\ old = [w \in Writers |-> "none"]
    /\ h0 = [w \in Writers |-> 0]
    /\ crashes = 0

SetPC(w, s) == pc' = [pc EXCEPT ![w] = s]

(* A request of this node arrives: the gate remembers the database head. *)
Arrive(w) ==
    /\ pc[w] = "idle"
    /\ Kind[w] \in {"rpc", "push"}
    /\ h0' = [h0 EXCEPT ![w] = Len(log)]
    /\ SetPC(w, "arriving")
    /\ UNCHANGED <<log, local, wm, notes, lock, old, crashes>>

(* The gate let the request in (wm >= head at arrival). An rpc decides: a  *)
(* land already in the branch (ancestry on the local tip) or recorded in   *)
(* the database is answered already_landed; otherwise it reads the tip.    *)
ReadRpc(w) ==
    /\ pc[w] = "arriving"
    /\ Kind[w] = "rpc"
    /\ wm >= h0[w]
    /\ LET b == Target[w] IN
       IF Land[w] /= NoLand
          /\ (Land[w] \in Contains(local[b]) \/ Land[w] \in DBLanded)
       THEN SetPC(w, "acked") /\ UNCHANGED old
       ELSE /\ old' = [old EXCEPT ![w] = local[b]]
            /\ SetPC(w, "read")
    /\ UNCHANGED <<log, local, wm, notes, lock, h0, crashes>>

(* A push's old value: what the server advertised (local), or the         *)
(* database's. Values are unique here, so the second choice stands for an *)
(* ABA on another node that brought the database back to the advertised   *)
(* value while this copy holds the value in between (with stock git the   *)
(* old value is always the advertised one; see dbfirst-writes.md).        *)
ReadPush(w) ==
    /\ pc[w] = "arriving"
    /\ Kind[w] = "push"
    /\ wm >= h0[w]
    /\ \E v \in {local[Target[w]], DB(Target[w])} :
          old' = [old EXCEPT ![w] = v]
    /\ SetPC(w, "read")
    /\ UNCHANGED <<log, local, wm, notes, lock, h0, crashes>>

(* Another node: reads the database and commits with a compare-and-swap. *)
RemoteRead(w) ==
    /\ pc[w] = "idle"
    /\ Kind[w] = "remote"
    /\ old' = [old EXCEPT ![w] = DB(Target[w])]
    /\ SetPC(w, "read")
    /\ UNCHANGED <<log, local, wm, notes, lock, h0, crashes>>

RemoteCommit(w) ==
    /\ pc[w] = "read"
    /\ Kind[w] = "remote"
    /\ IF ~GuardCAS \/ DB(Target[w]) = old[w]
       THEN CommitOf(w, old[w]) /\ SetPC(w, "acked")
       ELSE SetPC(w, "idle") /\ UNCHANGED log
    /\ UNCHANGED <<local, wm, notes, lock, old, h0, crashes>>

-----------------------------------------------------------------------------
(* DB-first rpc writer: the database commit is the whole write. No git     *)
(* lock, no local write; the applier brings the local copy up.             *)
DBFirstCommit(w) ==
    /\ DBFirst
    /\ Kind[w] = "rpc"
    /\ pc[w] = "read"
    /\ IF ~GuardCAS \/ DB(Target[w]) = old[w]
       THEN CommitOf(w, old[w]) /\ SetPC(w, "committed")
       ELSE SetPC(w, "idle") /\ UNCHANGED log     \* lost race: retry
    /\ UNCHANGED <<local, wm, notes, lock, old, h0, crashes>>

(* The RPC answers once the database committed (the local apply is not    *)
(* waited for in this model: the gate gives read-your-writes).            *)
AckDBFirst(w) ==
    /\ pc[w] = "committed"
    /\ SetPC(w, "acked")
    /\ UNCHANGED <<log, local, wm, notes, lock, old, h0, crashes>>

-----------------------------------------------------------------------------
(* The hook path: pushes, and rpc writers without DBFirst.                 *)
HookPath(w) == Kind[w] = "push" \/ (Kind[w] = "rpc" /\ ~DBFirst)

TakeLock(w) ==
    /\ pc[w] = "read"
    /\ HookPath(w)
    /\ lock = "free"
    /\ lock' = w
    /\ SetPC(w, "locked")
    /\ UNCHANGED <<log, local, wm, notes, old, h0, crashes>>

(* git's prepare: the old value against the local reference, under the     *)
(* lock. A non-atomic push keeps a rejected update in its transaction and  *)
(* feeds it to the hook; update-ref (rpc) fails and the land retries.      *)
GitPrepare(w) ==
    /\ pc[w] = "locked"
    /\ IF local[Target[w]] = old[w]
       THEN SetPC(w, "prepared") /\ UNCHANGED lock
       ELSE IF Kind[w] = "push"
            THEN SetPC(w, "gitRejected") /\ UNCHANGED lock
            ELSE SetPC(w, "idle") /\ lock' = "free"
    /\ UNCHANGED <<log, local, wm, notes, old, h0, crashes>>

(* The voter's Prepared phase: the database commit while git holds the lock. *)
VoterCommit(w) ==
    /\ pc[w] \in {"prepared", "localfirst"}
    /\ IF ~GuardCAS \/ DB(Target[w]) = old[w]
       THEN /\ CommitOf(w, old[w])
            /\ IF pc[w] = "localfirst"
               THEN /\ notes' = notes \cup {Len(log) + 1}
                    /\ lock' = "free"
                    /\ SetPC(w, "acked")
               ELSE SetPC(w, "dbdone") /\ UNCHANGED <<lock, notes>>
       ELSE /\ UNCHANGED <<log, notes>>
            /\ lock' = "free"
            /\ SetPC(w, IF Kind[w] = "rpc" THEN "idle" ELSE "rejected")
    /\ UNCHANGED <<local, wm, old, h0, crashes>>

(* Bug variant (GuardDBBeforeLocal = FALSE): git writes before the voter.  *)
GitCommitEarly(w) ==
    /\ ~GuardDBBeforeLocal
    /\ pc[w] = "prepared"
    /\ local' = [local EXCEPT ![Target[w]] = w]
    /\ SetPC(w, "localfirst")
    /\ UNCHANGED <<log, wm, notes, lock, old, h0, crashes>>

(* The voter's Prepared phase for an update git rejected. The voter cannot  *)
(* see the rejection; it sees the local value (one for-each-ref). With a    *)
(* zero old value and an existing local reference the update looks forced.  *)
(* If the database commits it, git writes nothing for it, the client is     *)
(* told it was rejected, and the Committed phase still notes the sequence.  *)
VoterOnRejected(w) ==
    /\ pc[w] = "gitRejected"
    /\ LET b           == Target[w]
           staleCreate == old[w] = "none" /\ local[b] /= "none"
           staleUpdate == old[w] /= "none" /\ local[b] /= old[w]
           refuse      == \/ GuardStaleCreate /\ staleCreate
                          \/ GuardStaleUpdate /\ (staleCreate \/ staleUpdate)
           ok          == ~refuse /\ (staleCreate \/ ~GuardCAS \/ DB(b) = old[w])
       IN IF ok
          THEN /\ CommitOf(w, IF staleCreate THEN "any" ELSE old[w])
               /\ notes' = notes \cup {Len(log) + 1}
          ELSE UNCHANGED <<log, notes>>
    /\ lock' = "free"
    /\ SetPC(w, "rejected")
    /\ UNCHANGED <<local, wm, old, h0, crashes>>

(* git writes the reference; Committed notes the sequence (NoteCommitted). *)
GitCommit(w) ==
    /\ pc[w] = "dbdone"
    /\ local' = [local EXCEPT ![Target[w]] = w]
    /\ notes' = notes \cup {SeqOfW(w)}
    /\ lock' = "free"
    /\ SetPC(w, "acked")
    /\ UNCHANGED <<log, wm, old, h0, crashes>>

-----------------------------------------------------------------------------
(* The watermark moves through contiguous noted sequences (advanceLocked). *)
Advance ==
    /\ (wm + 1) \in notes
    /\ wm' = wm + 1
    /\ notes' = notes \ {wm + 1}
    /\ UNCHANGED <<log, local, lock, pc, old, h0, crashes>>

(* The applier (catch-up): applies changesets wm+1..k as one transaction  *)
(* under git's lock, then writes the watermark. A reference whose local   *)
(* value is newer than the range (a hook write of this node) is kept.     *)
Apply ==
    /\ lock = "free"
    /\ wm < Len(log)
    /\ \E k \in (wm + 1)..Len(log) :
          /\ local' = [b \in Branches |->
                         IF LastWrite(b, k) <= wm THEN local[b]
                         ELSE IF GuardNeverRevert /\ SeqOfValue(local[b]) > k
                              THEN local[b]
                              ELSE ValueAt(b, k)]
          /\ wm' = k
          /\ notes' = {s \in notes : s > k}
    /\ UNCHANGED <<log, lock, pc, old, h0, crashes>>

-----------------------------------------------------------------------------
(* Failures of this node. Remote writers are on another node.             *)
AfterCrash ==
    [w \in Writers |->
        IF Kind[w] = "remote" \/ pc[w] \in Terminal \cup {"idle"} THEN pc[w]
        ELSE IF Committed(w) THEN "unknown" ELSE "failed"]

(* Process crash: git's lock and the in-memory notes are gone; the local  *)
(* references and the watermark survive in the page cache.                *)
Crash ==
    /\ crashes < MaxCrashes
    /\ crashes' = crashes + 1
    /\ lock' = "free"
    /\ notes' = {}
    /\ pc' = AfterCrash
    /\ UNCHANGED <<log, local, wm, old, h0>>

(* Power loss: the dirty copy is moved aside and hydrated at the head.    *)
PowerLoss ==
    /\ crashes < MaxCrashes
    /\ crashes' = crashes + 1
    /\ lock' = "free"
    /\ notes' = {}
    /\ pc' = AfterCrash
    /\ local' = [b \in Branches |-> DB(b)]
    /\ wm' = Len(log)
    /\ UNCHANGED <<log, old, h0>>

-----------------------------------------------------------------------------
WriterStep(w) ==
    \/ Arrive(w) \/ ReadRpc(w) \/ ReadPush(w) \/ RemoteRead(w)
    \/ RemoteCommit(w) \/ DBFirstCommit(w) \/ AckDBFirst(w)
    \/ TakeLock(w) \/ GitPrepare(w) \/ VoterCommit(w) \/ GitCommitEarly(w)
    \/ VoterOnRejected(w) \/ GitCommit(w)

Next ==
    \/ \E w \in Writers : WriterStep(w)
    \/ Advance
    \/ Apply
    \/ Crash
    \/ PowerLoss

Fairness ==
    /\ \A w \in Writers : WF_vars(WriterStep(w))
    /\ WF_vars(Advance)
    /\ WF_vars(Apply)

Spec == Init /\ [][Next]_vars /\ Fairness

-----------------------------------------------------------------------------
(* Invariants *)

TypeOK ==
    /\ local \in [Branches -> Values]
    /\ wm \in 0..Len(log)
    /\ notes \subseteq 1..Len(log)
    /\ lock \in Writers \cup {"free"}
    /\ pc \in [Writers -> PCs]
    /\ old \in [Writers -> Values]

(* The log is one dense, ordered sequence. *)
LogDense == \A i \in 1..Len(log) : log[i].seq = i

(* Local references are never ahead of the database: every local value is *)
(* the database value of that reference at some changeset.                *)
LocalNotAhead ==
    \A b \in Branches : \E s \in 0..Len(log) : local[b] = ValueAt(b, s)

(* The watermark claims only what is applied: every reference is at least *)
(* at its last write up to wm.                                            *)
WatermarkTruthful ==
    \A b \in Branches : SeqOfValue(local[b]) >= LastWrite(b, wm)

(* An acknowledged write is in the database (an already_landed answer     *)
(* means the land is).                                                    *)
AckedDurable ==
    \A w \in Writers :
        pc[w] = "acked" =>
            (Committed(w) \/ (Land[w] /= NoLand /\ Land[w] \in DBLanded))

(* No lost update per branch: every changeset found the value it expected. *)
NoLostUpdate == \A i \in 1..Len(log) : log[i].exp = log[i].old

(* A write the client was told was refused is not in the database. *)
NoPhantom == \A w \in Writers : pc[w] = "rejected" => ~Committed(w)

(* Exactly-once land: a land identity is in at most one changeset. *)
ExactlyOnceLand ==
    \A i, j \in 1..Len(log) :
        (i /= j /\ log[i].land /= NoLand) => log[i].land /= log[j].land

(* Read-your-writes: a request admitted by the gate after an acknowledged *)
(* write (wm >= head at its arrival >= the write's changeset) sees it.     *)
ReadYourWrites ==
    \A w \in Writers :
        (pc[w] = "acked" /\ Committed(w) /\ wm >= SeqOfW(w))
            => SeqOfValue(local[Target[w]]) >= SeqOfW(w)

(* Liveness: once the writers are done, the local copy equals the database. *)
EventuallyConsistent ==
    <>[](wm = Len(log) /\ \A b \in Branches : local[b] = DB(b))

-----------------------------------------------------------------------------
(* Scenarios, selected by the .cfg files (Kind <- KindS1 and so on).       *)

\* S1: two rpc writers land the same change on b1 (a client retry), a push
\* on b1, and another node writing b2.
KindS1   == [w \in Writers |-> CASE w = "w3" -> "push"
                                  [] w = "w4" -> "remote"
                                  [] OTHER    -> "rpc"]
TargetS1 == [w \in Writers |-> IF w = "w4" THEN "b2" ELSE "b1"]
LandS1   == [w \in Writers |-> IF w \in {"w1", "w2"} THEN "l1" ELSE NoLand]
InitS1   == [b \in Branches |-> "init"]

\* S2: two pushes on b1 while another node writes b2 (the catch-up runs
\* while the hook path writes locally).
KindS2   == [w \in Writers |-> IF w = "w3" THEN "remote" ELSE "push"]
TargetS2 == [w \in Writers |-> IF w = "w3" THEN "b2" ELSE "b1"]
LandS2   == [w \in Writers |-> NoLand]
InitS2   == [b \in Branches |-> "init"]

\* S3: a push creates b2 while an rpc creates it too.
KindS3   == [w \in Writers |-> IF w = "w1" THEN "push" ELSE "rpc"]
TargetS3 == [w \in Writers |-> "b2"]
LandS3   == [w \in Writers |-> NoLand]
InitS3   == [b \in Branches |-> IF b = "b2" THEN "none" ELSE "init"]

\* S4: a push on b1 while another node writes b1 (the current design,
\* several nodes).
KindS4   == [w \in Writers |-> IF w = "w2" THEN "remote" ELSE "push"]
TargetS4 == [w \in Writers |-> "b1"]
LandS4   == [w \in Writers |-> NoLand]
InitS4   == [b \in Branches |-> "init"]
=============================================================================
