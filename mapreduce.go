/*
Package mapreduce implements a MapReduce library.
*/

package gomapreduce

import (
	"net"          // interface for network I/O
	"net/rpc"
	"log"
	"os"
	"syscall"
	"sync"
	"fmt"
	"math/rand"
	//"math"
	"time"
	"encoding/gob"
)

type MapReduceNode struct {
	mu sync.Mutex         // singleton mutex for node
	l net.Listener        // RPC network listener
	dead bool             // used for testing dead nodes
	unreliable bool       // used for testing unreliable nodes
	rpcCount int          // maintain count of RPC calls

	me int                // index into nodes
	nodes []string        // MapReduceNode port names
	node_count int
	net_mode string       // "unix" or "tcp"

	// State for master role
	jobs map[string] Job   // Maps string job_id -> Job
  tm TaskManager        // all Tasks the MapReduceNode while acting as a master

	// State for worker roles
  intermediates map[MediateTuple][]Pair
  // Last ping times for each other node
  lastPingTimes map[string]time.Time
  // State ("idle", "dead", etc.) of each other node
  nodeStates map[string]string
}


/*
Client would like to start a Job instance which is composed of Task 
instances (MapTasks or Reduce Tasks). Client passes a JobConfig instance along
with his implemented Mapper, Reducer, InputAccessor, and OutputAccessor.
Spawns a master_role thread to perform the requested Job by breaking it into 
tasks that are allocated to workers. Returns the int job_id assigned to the 
started Job.
Any configuration settings not for a particular job should be read from the 
environment.

Aside: Currently, this is called from a client which has a local MapReduceNode running
at it, but a wrapper that allows start to be called remotely via RPC could be 
created. We don't currently have any scenarios where the client is not also a 
member of the network but it is totally possible.
*/
func (self *MapReduceNode) Start(job_config JobConfig, mapper Mapper, 
  reducer Reducer, inputer InputAccessor, intermediateAccessor IntermediateAccessor, outputer OutputAccessor) string {

  //self.broadcast_testrpc(mapper)          // temporary

  job_id := generate_uuid()       // Job identifier created internally, unlike in Paxos
  job := makeJob(job_id, "starting", self.me, mapper, reducer, inputer, intermediateAccessor, outputer)
  self.jobs[job.getId()] = job

  debug(fmt.Sprintf("(svr:%d) Start: job_id: %s, job: %v", self.me, job_id, job))

  // Spawn a thread to act as the master
	go self.masterRole(job, job_config)

  return job_id
}


/*
Performs the requested Job by breaking it into tasks based on the JobConfig,
allocating the Tasks to workers, and monitors progress on the Job until it is 
complete.
The method used by the master node to start the entire mapreduce operation
*/
func (self *MapReduceNode) masterRole(job Job, config JobConfig) {
	debug(fmt.Sprintf("(svr:%d) master_role: job", self.me))
  jobId := job.getId()
  // MapTasks
  // Split input data into M components. Right now, input is prechunked so do nothing.
  maptasks := self.makeMapTasks(job, config)    // Create M MapTasks
  self.tm.addBulkMapTasks(jobId, maptasks)      // Add tasks to TaskManager
  self.assignTasks(jobId)                       // Assign unassigned MapTasks to workers
  self.awaitTasks(jobId, "assigned", "map")     // Wait for MapTasks to be completed

  // temporary debugging
  fmt.Printf("Unassigned: %s\n", self.tm.listTasks(jobId, "unassigned", "all"))
  fmt.Printf("Assigned: %s\n", self.tm.listTasks(jobId, "assigned", "all"))
  fmt.Printf("Completed: %s\n", self.tm.listTasks(jobId, "completed", "all"))

  // ReduceTasks (+ failed MapTasks )
  // reducetasks := makeReduceTasks(job, config)
  // self.tm.addBulkReduceTasks(job.getId(), config)
  // self.assignTasks(job)
  // self.awaitTasks("all")                          // Wait for MapTasks and ReduceTasks to be completed

  // // Cleanup
  // self.tm.jobDone(job.getId())
}

func (self *MapReduceNode) tick() {
  //fmt.Println("Tick")
  self.sendPings()
	self.checkForDisconnectedNodes()

}


// func (self *MapReduceNode) broadcast_testrpc(maptask Mapper) {
//   fmt.Println(self.nodes, maptask)
//   for index, node := range self.nodes {
//     if index == self.me {
//       continue
//     }
//     args := &TestRPCArgs{}         // declare and init zero valued struct
//     args.Number = rand.Int()
//     //task := ExampleMapper{}
//     args.Mapper = maptask
//     var reply TestRPCReply
//     ok := self.call(node, "MapReduceNode.TestRPC", args, &reply)
//     if ok {
//       fmt.Println("Successfully sent")
//       fmt.Println(reply)
//     } else {
//       fmt.Println("Sent but not received")
//       fmt.Println(reply)
//     }
//   }
//   return
// }


// Handle test RPC RPC calls.
// func (self *MapReduceNode) TestRPC(args *TestRPCArgs, reply *TestRPCReply) error {
//   fmt.Println("Received TestRPC", args.Number)
//   result := args.Mapper.Map("This is a sample string sample string is is")       // perform work on a random input
//   fmt.Println(result)
//   //fmt.Printf("Task id: %d\n", args.Mapper.get_id())
//   reply.Err = OK
//   return nil
// }


// Exported RPC functions (internal to mapreduce service)
///////////////////////////////////////////////////////////////////////////////

// Accepts a request to perform a MapTask or an AcceptTask. May decline the
// request if overworked
// A method used by a map worker. The worker will fetch the data associated with the key for the job it's assigned, and
// then run the map function on that data. The worker stores the intermediate key/value pairs in memory and tells the
// master where those values are stored so that reduce workers can get them when needed.
func (self *MapReduceNode) ReceiveTask(args *AssignTaskArgs, reply *AssignTaskReply) error {
	self.mu.Lock()
  defer self.mu.Unlock()

  debug(fmt.Sprintf("(svr:%d) ReceiveTask: %v", self.me, args))
  // TODO: run as a worker thread
  args.Task.execute()
	// TODO Write the intermediate keys/values to somewhere (in memory for now) so it can be fetched by reducers later

	reply.OK = true
	return nil
}

/*

*/
func (self *MapReduceNode) Get() {
    debug(fmt.Sprintf("(svr:%d) Get", self.me))

}


// Helpers
///////////////////////////////////////////////////////////////////////////////

// Gets all the keys that need to be mapped via MapTasks for the job and 
// constructs MapTask instances. Returns a slice of MapTasks.
func (self *MapReduceNode) makeMapTasks(job Job, config JobConfig) []MapTask {
  var task_list []MapTask

  // Assumes the Job input is prechunked
	for _, key := range job.inputer.ListKeys() {
    task_id := generate_uuid()
    maptask := makeMapTask(task_id, self.me, key, job.mapper, job.inputer, job.intermediateAccessor)
    task_list = append(task_list, maptask)
	}
	return task_list
}

/*
Synchronous function used by the master thread to assign Tasks to workers. 
Returns when all Tasks for the given jobId in the TaskManager have been assigned.
*/
func (self *MapReduceNode) assignTasks(jobId string) {
  var worker string             // MapReducerNode port
  var task Task
 
  num_unassigned := self.tm.countTasks(jobId, "unassigned", "all")
  for num_unassigned > 0 {
    debug(fmt.Sprintf("(svr:%d) Unassigned: %v", self.me, num_unassigned))
    taskIds := self.tm.listTasks(jobId, "unassigned", "all")

    // Assign unassigned Tasks
    for _, taskId := range taskIds {
      taskState := self.tm.getTaskStateCopy(jobId, taskId)
      task = taskState.task
      args := AssignTaskArgs{Task: task}
      var reply AssignTaskReply
      worker = self.nodes[taskState.workerIndex]

      ok := self.call(worker, "MapReduceNode.ReceiveTask", args, &reply)
      if ok && reply.OK {
        // Worker accepted the Task assignment
        self.tm.setTaskStatus(jobId, task.getId(), "assigned")
      }
    }

    num_unassigned = self.tm.countTasks(jobId, "unassigned", "all")
    time.Sleep(100 * time.Millisecond) 
  }
}

// moved getNumberUnfinished into Task manager

/*
Synchronous function checks that all Tasks of the given JobId and of the given
type have been marked as 'complete' and returns only when they have all been 
marked 'complete'
*/
func (self *MapReduceNode) awaitTasks(jobId string, status string, kind string) {

}

// Helper method for a node to send a ping to all of its peers
func (self *MapReduceNode) sendPings() {
	args := &PingArgs{self.nodes[self.me]}
	for _, node := range self.nodes {
		if node != self.nodes[self.me]{
			reply := &PingReply{}
			self.call(node, "MapReduceNode.HandlePing", args, reply)
		}
	}
}

// Ping handler, called via RPC when a node wants to ping this node
func (self *MapReduceNode) HandlePing(args *PingArgs, reply *PingReply) error {
	// fmt.Printf("Node %d receiving ping from Node %s\n", self.me, args.Me[len(args.Me)-1:])

	self.lastPingTimes[args.Me] = time.Now()

	return nil
}

// checks to see when each of the other nodes last pinged. If it was too long ago, mark them as dead
func (self *MapReduceNode) checkForDisconnectedNodes() {
	for node, lastPingTime := range self.lastPingTimes {
		timeDifference := time.Now().Sub(lastPingTime)
		if timeDifference > DeadPings * PingInterval {
			fmt.Printf("Node %d marking node %s as dead\n", self.me, node)
			self.nodeStates[node] = "dead"
		}
	}
}


// Handle test RPC RPC calls.
// func (self *MapReduceNode) TestRPC(args *TestRPCArgs, reply *TestRPCReply) error {
//   fmt.Println("Received TestRPC", args.Number)
//   result := args.Mapper.Map("This is a sample string sample string is is")       // perform work on a random input
//   fmt.Println(result)
//   //fmt.Printf("Task id: %d\n", args.Mapper.get_id())
//   reply.Err = OK
//   return nil
// }



//
// call() sends an RPC to the rpcname handler on server srv
// with arguments args, waits for the reply, and leaves the
// reply in reply. the reply argument should be a pointer
// to a reply structure.
//
// the return value is true if the server responded, and false
// if call() was not able to contact the server. in particular,
// the reply's contents are only valid if call() returned true.
//
// you should assume that call() will time out and return an
// error after a while if it doesn't get a reply from the server.
//
// please use call() to send all RPCs, in client.go and server.go.
// please don't change this function.
//
func (self *MapReduceNode) call(srv string, rpcname string, args interface{}, reply interface{}) bool {
	c, errx := rpc.Dial("unix", srv)
	if errx != nil {
		fmt.Printf("Error: %s\n", errx)
		return false
	}
	defer c.Close()
		
	err := c.Call(rpcname, args, reply)
	if err == nil {
		return true
	}
	fmt.Printf("Error: %s\n", err)
	return false
}  


//
// tell the peer to shut itself down.
// for testing.
// please do not change this function.
//
func (self *MapReduceNode) Kill() {
	self.dead = true
	if self.l != nil {
		self.l.Close()
	}
}


// Create an MapReduceNode instance
// The ports of all the nodes (including this one) are in nodes[], 
// this node's port is nodes[me]

func Make(nodes []string, me int, rpcs *rpc.Server, mode string) *MapReduceNode {
  // Initialize a MapReduceNode
  mr := &MapReduceNode{}
  mr.nodes = nodes    
  mr.me = me
  mr.net_mode = mode
  // Initialization code
  mr.node_count = len(nodes)
  mr.jobs = make(map[string]Job)
  mr.tm = makeTaskManager(len(nodes))

  // Initialize last ping times for each node
  mr.nodeStates = map[string]string{}
  mr.lastPingTimes = map[string]time.Time{}
  for _, node := range mr.nodes {
  	if node == mr.nodes[mr.me] { 	// Skip ourself
  		continue
  	}
  	mr.lastPingTimes[node] = time.Now()
  	mr.nodeStates[node] = "idle"
  }

  if rpcs != nil {
    rpcs.Register(mr)      // caller created RPC Server
  } else {
    rpcs = rpc.NewServer() // creates a new RPC server
    rpcs.Register(mr)      // Register exported methods of MapReduceNode with RPC Server         
    gob.Register(TestRPCArgs{})
    gob.Register(TestRPCReply{})
    gob.Register(MapTask{})
    gob.Register(ReduceTask{})
    gob.Register(S3Accessor{})
    gob.Register(SimpleIntermediateAccessor{})

    // Prepare node to receive connections

    if mode == "tcp" {
      fmt.Println("Making in TCP mode")
      listener, error := net.Listen("tcp", ":8080");

      if error != nil {
        log.Fatal("listen error: ", error);
      }
      mr.l = listener      // Set MapReduceNode listener
      go func() {
        for mr.dead == false {
          conn, err := mr.l.Accept()
          if err == nil && mr.dead == false {
            if mr.unreliable && (rand.Int63() % 1000) < 100 {
              // discard the request.
              conn.Close()
            } else if mr.unreliable && (rand.Int63() % 1000) < 200 {
              // process the request but force discard of reply.
              c1 := conn.(*net.UnixConn)
              f, _ := c1.File()
              err := syscall.Shutdown(int(f.Fd()), syscall.SHUT_WR)
              if err != nil {
                fmt.Printf("shutdown: %v\n", err)
              }
              mr.rpcCount++
              go rpcs.ServeConn(conn)
            } else {
              mr.rpcCount++
              go rpcs.ServeConn(conn)
            }
          } else if err == nil {
            conn.Close()
          }
          if err != nil && mr.dead == false {
            fmt.Printf("Paxos(%v) accept: %v\n", me, err.Error())
          }
        }
      }()

    } else {
      fmt.Println("Making in Unix mode")
      // mode assumed to be "unix"

      os.Remove(nodes[me]) // only needed for "unix"
      listener, error := net.Listen("unix", nodes[me]);
      if error != nil {
        log.Fatal("listen error: ", error);
      }
      mr.l = listener      // Set MapReduceNode listener

      

      // please do not change any of the following code,
      // or do anything to subvert it.
      
      // create a thread to accept RPC connections
      go func() {
        for mr.dead == false {
          conn, err := mr.l.Accept()
          if err == nil && mr.dead == false {
            if mr.unreliable && (rand.Int63() % 1000) < 100 {
              // discard the request.
              conn.Close()
            } else if mr.unreliable && (rand.Int63() % 1000) < 200 {
              // process the request but force discard of reply.
              c1 := conn.(*net.UnixConn)
              f, _ := c1.File()
              err := syscall.Shutdown(int(f.Fd()), syscall.SHUT_WR)
              if err != nil {
                fmt.Printf("shutdown: %v\n", err)
              }
              mr.rpcCount++
              go rpcs.ServeConn(conn)
            } else {
              mr.rpcCount++
              go rpcs.ServeConn(conn)
            }
          } else if err == nil {
            conn.Close()
          }
          if err != nil && mr.dead == false {
            fmt.Printf("Paxos(%v) accept: %v\n", me, err.Error())
          }
        }
      }()
    }

  }

  go func() {
    for mr.dead == false {
      mr.tick()
      time.Sleep(250 * time.Millisecond)
    }
  }()

  return mr
}

