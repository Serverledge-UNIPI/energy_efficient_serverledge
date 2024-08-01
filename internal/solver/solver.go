package solver

/*
#cgo CFLAGS: -I/usr/include/python3.10
#cgo LDFLAGS: -lpython3.10
#include <Python.h>
#include <stdlib.h>

extern void initializePython();
extern void finalizePython();
extern int* allocateMemory(int size);
extern void freeMemory(int* arr);
extern const char* startSolver(int numberOfNodes, int numberOfFunctions, int* nodeMemory, int* nodeCapacity, int* maximumCapacity, int* nodeIpc, int* nodePowerConsumption, int* functionMemory, int* functionWorkload, int* functionDeadline, int* functionInvocations);
*/
import "C"
import (
	"encoding/json"
	"log"
	"time"
	"fmt"
	"math"
	"errors"
	"unsafe"

	"github.com/grussorusso/serverledge/internal/config"
	"github.com/grussorusso/serverledge/internal/registration"
	"github.com/grussorusso/serverledge/internal/node"
	"github.com/grussorusso/serverledge/internal/function"
	"github.com/grussorusso/serverledge/utils"

	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/mem"

	clientv3 "go.etcd.io/etcd/client/v3"
	"golang.org/x/net/context"
)

func Run() {
	err := initNodeResources()
	if err != nil {
		log.Fatalf("Error in initializing node resources: %v", err)
		return
	}

	isSolverNode := config.GetBool(config.IS_SOLVER_NODE, false)

	if isSolverNode {
		epochDuration := config.GetInt(config.EPOCH_DURATION, 30)
		solverTicker := time.NewTicker(time.Duration(epochDuration) * time.Second) // TODO: time.Minute
		defer solverTicker.Stop()

		for {
			select {
			case <-solverTicker.C:
				solve()
			}
		}
	} else {
		watchAllocation()
	}
}

func watchAllocation() {
	log.Println("Running watcher for allocation")
	etcdClient, err := utils.GetEtcdClient()
	if err != nil {
		log.Fatal(err)
		return
	}

    watchChan := etcdClient.Watch(context.Background(), "allocation")
    for watchResp := range watchChan {
        for _, event := range watchResp.Events {
            log.Printf("Event received! Type: %s Key: %s Value: %s\n", event.Type, event.Kv.Key, event.Kv.Value)

			// Update functions allocation
			allocation, err := getAllocationFromEtcd()
			if err != nil {
				log.Printf("Error retrieving allocation: %v\n", err)
				continue
			}

			setAllocation(allocation)
			log.Printf("Updated Allocation: %v\n", Allocation)
        }
    }
}

func solve() {
	log.Println("Running solver")
	
	// Get all available servers and functions
	serversMap := registration.GetServersMap()
	functions, err := function.GetAll()
	if err != nil {
		log.Fatalf("Error retrieving functions: %v", err)
		return
	}

	var numberOfNodes int = len(serversMap) + 1
	var numberOfFunctions int = len(functions)

	if numberOfNodes == 0 || numberOfFunctions == 0 {
		return
	}

	// Prepare data slices
	nodeInfo, nodeIp := prepareNodeInfo(serversMap)
	functionInfo := prepareFunctionInfo(functions)

	// Initialize Python interpreter
	C.initializePython()
	//defer C.finalizePython()

	// Allocate and initialize memory for C arrays
	cNodeInfo := allocateAndInitialize(nodeInfo.TotalMemoryMB)
	defer C.freeMemory(cNodeInfo)
	cComputationalCapacity := allocateAndInitialize(nodeInfo.ComputationalCapacity)
	defer C.freeMemory(cComputationalCapacity)
	cMaximumCapacity := allocateAndInitialize(nodeInfo.MaximumCapacity)
	defer C.freeMemory(cMaximumCapacity)
	cIPC := allocateAndInitialize(nodeInfo.IPC)
	defer C.freeMemory(cIPC)
	cPowerConsumption := allocateAndInitialize(nodeInfo.PowerConsumption)
	defer C.freeMemory(cPowerConsumption)

	cFunctionMemory := allocateAndInitialize(functionInfo.MemoryMB)
	defer C.freeMemory(cFunctionMemory)
	cFunctionWorkload := allocateAndInitialize(functionInfo.Workload)
	defer C.freeMemory(cFunctionWorkload)
	cFunctionDeadline := allocateAndInitialize(functionInfo.Deadline)
	defer C.freeMemory(cFunctionDeadline)
	cFunctionInvocations := allocateAndInitialize(functionInfo.Invocations)
	defer C.freeMemory(cFunctionInvocations)

	cResults := C.startSolver(
		C.int(numberOfNodes),
		C.int(numberOfFunctions),
		cNodeInfo,
		cComputationalCapacity,
		cMaximumCapacity,
		cIPC,
		cPowerConsumption,
		cFunctionMemory,
		cFunctionWorkload,
		cFunctionDeadline,
		cFunctionInvocations,
	)

	// Process solver results
	jsonStr := C.GoString(cResults)

	var results SolverResults
	if err := json.Unmarshal([]byte(jsonStr), &results); err != nil {
		log.Fatalf("Error unmarshalling results: %v", err)
		return
	}

	// Log results
	log.Printf("Solver walltime: %f", results.SolverWalltime)
	log.Printf("Solver status: %s", results.SolverStatusName)
	log.Printf("Energy consumption: %f", results.ObjectiveValue)
	log.Printf("Active nodes: %v", results.ActiveNodesIndexes)
	log.Printf("Functions capacity: %v", results.FunctionsCapacity)

	for nodeID, instances := range results.NodesInstances {
		log.Printf("Node %d has instances: %v", nodeID, instances)
	}

	log.Printf("Node IP addresses: %v", nodeIp)

	// Retrive functions allocation
	allocation, err := computeFunctionsAllocation(results, functions, nodeIp)
	if err != nil {
		log.Fatalf("Error processing allocation: %v", err)
		return
	}

	// Save allocation to Etcd
	if err := saveAllocationToEtcd(allocation); err != nil {
		log.Fatalf("Error saving allocation to Etcd: %v", err)
	}

	log.Println("Solver terminated")
}

func prepareNodeInfo(serversMap map[string]*registration.StatusInformation) (NodeInformation, []string) {
	nodeIp := make([]string, len(serversMap) + 1)
	nodeInfo := NodeInformation{
		TotalMemoryMB:			make([]int, len(serversMap) + 1),
		ComputationalCapacity:	make([]int, len(serversMap) + 1),
		MaximumCapacity:      	make([]int, len(serversMap) + 1),
		IPC:              		make([]int, len(serversMap) + 1),
		PowerConsumption: 		make([]int, len(serversMap) + 1),
	}

	i := 0
	for _, server := range serversMap {
        nodeInfo.TotalMemoryMB[i] = int(server.TotalMemoryMB)
        nodeInfo.ComputationalCapacity[i] = int(server.ComputationalCapacity)
        nodeInfo.MaximumCapacity[i] = int(server.MaximumCapacity)
        nodeInfo.IPC[i] = int(server.IPC * 10)
        nodeInfo.PowerConsumption[i] = int(server.PowerConsumption)

        // Get node IP address
        nodeIp[i] = server.Url[7:len(server.Url) - 5]
		i++
    }

    nodeInfo.TotalMemoryMB[i] = int(node.Resources.TotalMemoryMB)
    nodeInfo.ComputationalCapacity[i] = int(node.Resources.ComputationalCapacity)
    nodeInfo.MaximumCapacity[i] = int(node.Resources.MaximumCapacity)
    nodeInfo.IPC[i] = int(node.Resources.IPC * 10)
    nodeInfo.PowerConsumption[i] = int(node.Resources.PowerConsumption)

	nodeIp[i] = utils.GetIpAddress().String()

	return nodeInfo, nodeIp
}

func prepareFunctionInfo(functions []string) FunctionInformation {
	functionInfo := FunctionInformation{
		MemoryMB:		make([]int, len(functions)),
		Workload:		make([]int, len(functions)),
		Deadline:		make([]int, len(functions)),
		Invocations:	make([]int, len(functions)),
	}

	for i, functionName := range functions {
		f, err := function.GetFunction(functionName)
		if !err {
			log.Printf("Error retrieving function %s: %v", functionName, err)
			continue
		}

		functionInfo.MemoryMB[i] = int(f.MemoryMB)
		functionInfo.Workload[i] = int(f.Workload / 1e6)
		functionInfo.Deadline[i] = int(f.Deadline)
		functionInfo.Invocations[i] = int(f.Invocations)
	}

	return functionInfo
}

func computeFunctionsAllocation(results SolverResults, functions []string, nodeIp []string) (FunctionsAllocation, error) {
	allocation := make(FunctionsAllocation)
	for i, functionName := range functions {
		ipInstancesMap := make(map[string]int)
		for key, instances := range results.NodesInstances {
			if floatVal, ok := instances[i].(float64); ok {
				ipInstancesMap[nodeIp[key]] = int(floatVal)
			} else {
				log.Printf("Expected float64 but found %T at index %d for nodeID %d", instances[i], i, key)
			}
		}

		allocation[functionName] = FunctionAllocation{
			Capacity:  results.FunctionsCapacity[i],
			Instances: ipInstancesMap,
		}

		f, err := function.GetFunction(functionName)
		if !err {
			return nil, errors.New("Function not found")
		}

		f.CPUDemand = math.Round((results.FunctionsCapacity[i] / node.Resources.MaximumCapacity) * 100) / 100
		if err := f.SaveToEtcd(); err != nil {
			return nil, err
		}
	}

	return allocation, nil
}

// Helper function to allocate and initialize C memory
func allocateAndInitialize(data []int) *C.int {
	size := len(data)
	cArray := C.allocateMemory(C.int(size))
	for i := 0; i < size; i++ {
		cElement := (*C.int)(unsafe.Pointer(uintptr(unsafe.Pointer(cArray)) + uintptr(i)*unsafe.Sizeof(*cArray)))
		*cElement = C.int(data[i])
	}
	return cArray
}

func initNodeResources() error {
	// Initialize node resources information
	cpuInfo, err := cpu.Info()
	if err != nil {
		log.Fatal(err)
		return err
	}

	vMemInfo, err := mem.VirtualMemory()
	if err != nil {
		log.Fatal(err)
		return err
	}

	node.Resources.ComputationalCapacity = cpuInfo[0].Mhz * float64(len(cpuInfo))
	node.Resources.MaximumCapacity = cpuInfo[0].Mhz
	node.Resources.IPC = 1 // TODO
	node.Resources.PowerConsumption = 400 // TODO
	node.Resources.TotalMemoryMB = int64(vMemInfo.Total / 1e6)

	return nil
}

func setAllocation(newAllocation FunctionsAllocation) {
    mu.Lock()
    defer mu.Unlock()
    Allocation = newAllocation
}

func GetAllocation() FunctionsAllocation {
    mu.RLock()
    defer mu.RUnlock()
    return Allocation
}

func saveAllocationToEtcd(allocation FunctionsAllocation) error {
	etcdClient, err := utils.GetEtcdClient()
	if err != nil {
		log.Fatal(err)
		return err
	}

	payload, err := json.Marshal(allocation)
	if err != nil {
		return fmt.Errorf("Could not marshal allocation: %v", err)
	}

	ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
	resp, err := etcdClient.Grant(ctx, 60) // TODO: lease time
	if err != nil {
		log.Fatal(err)
		return err
	}

	_, err = etcdClient.Put(ctx, "allocation", string(payload), clientv3.WithLease(resp.ID))
	if err != nil {
		log.Fatal(err)
		return err
	}

	return nil
}

func getAllocationFromEtcd() (FunctionsAllocation, error) {
	etcdClient, err := utils.GetEtcdClient()
	if err != nil {
		log.Fatal(err)
		return nil, err
	}

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    resp, err := etcdClient.Get(ctx, "allocation")
    if err != nil {
        return nil, fmt.Errorf("Failed to get allocation from etcd: %v", err)
    }

    if len(resp.Kvs) == 0 {
        return nil, fmt.Errorf("No data found for key 'allocation'")
    }

    var allocation FunctionsAllocation
    err = json.Unmarshal(resp.Kvs[0].Value, &allocation)
    if err != nil {
        return nil, fmt.Errorf("Failed to unmarshal allocation: %v", err)
    }

    return allocation, nil
}
