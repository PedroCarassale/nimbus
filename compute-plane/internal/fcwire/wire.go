// Package fcwire is the file the host writes on the read-only code drive
// and the guest init reads before it execs the runtime bootstrap.
// The JSON-line invoke protocol itself is unchanged from the warm containers.
package fcwire

// VsockPort is the guest port the host connects to. Firecracker has no NIC;
// this is the only channel, the same role stdin/stdout has under Docker.
const VsockPort = 5000

// ConfigName is the file at the root of the code drive, next to task/.
const ConfigName = "nimbus.json"

// TaskDir is the directory on the code drive that is bind-mounted at /var/task.
const TaskDir = "task"

// Config is one microVM's process setup. Env holds user variables and the
// platform variables (NIMBUS_HANDLER, NIMBUS_WARM, the trace id). The values
// are on the host disk while the VM exists, the same window docker inspect has.
type Config struct {
	Argv []string          `json:"argv"`
	Env  map[string]string `json:"env"`
	UID  int               `json:"uid"`
	GID  int               `json:"gid"`
}
