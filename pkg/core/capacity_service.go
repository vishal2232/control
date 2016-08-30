package core

import (
	"fmt"
	"strings"
	"time"

	"github.com/supergiant/guber"
	"github.com/supergiant/supergiant/pkg/models"
)

type CapacityService struct {
	core *Core
}

func (s *CapacityService) Perform() error {
	var kubes []*models.Kube
	if err := s.core.DB.Where("ready = ?", true).Preload("CloudAccount").Find(&kubes); err != nil {
		return err
	}

	// TODO
	// TODO
	// TODO
	// TODO
	// TODO
	//
	// 1. Concurrency        -- inParallel? probably shouldn't be on collection
	// 2. "scaling" should be an action on Kube, so we can see the status (actually that may not make sense, just use Nodes ?)

	for _, kube := range kubes {
		if err := newKubeScaler(s.core, kube).Scale(); err != nil {
			return err
		}
	}

	return nil
}

//------------------------------------------------------------------------------

var (
	waitBeforeScale         = 2 * time.Minute
	minAgeToExist           = 20 * time.Minute // this is used to prevent adding more nodes while still-pending pods are scheduling to a new node
	maxClusteredPodsPerNode = 2                // prevent putting all nodes of a cluster on one host node
	maxDisksPerNode         = 11
	trackedEventMessages    = [...]string{
		"MatchNodeSelector",
		"PodExceedsMaxPodNumber",
		"PodExceedsFreeMemory",
		"PodExceedsFreeCPU",
		"no nodes available to schedule pods",
	}
)

//------------------------------------------------------------------------------

type KubeScaler struct {
	core            *Core
	kube            *models.Kube
	nodeSizes       []*NodeSize
	largestNodeSize *NodeSize
}

func newKubeScaler(c *Core, kube *models.Kube) *KubeScaler {
	s := &KubeScaler{core: c, kube: kube}
	// We iterate on all nodeSizes here first to preserve the cost order
	for _, it := range c.NodeSizes[kube.CloudAccount.Provider] {
		for _, nodeSizeID := range kube.NodeSizes {
			if it.Name == nodeSizeID {
				s.nodeSizes = append(s.nodeSizes, it)
				break
			}
		}
	}
	s.largestNodeSize = s.nodeSizes[len(s.nodeSizes)-1]
	return s
}

//------------------------------------------------------------------------------

func (s *KubeScaler) Scale() error {
	incomingPods, err := s.incomingPods()
	if err != nil {
		return fmt.Errorf("Capacity service error when fetching incoming pods: %s", err)
	}

	var projectedNodes []*projectedNode
	for _, pod := range incomingPods {
		projectedNodes = append(projectedNodes, &projectedNode{
			false,
			s.largestNodeSize,
			[]*guber.Pod{pod},
		})
	}

	for {
		var (
			pnode1      *projectedNode
			pnode2      *projectedNode
			pnode2Index int
		)

		//==========================================================================
		// find an uncommitted nodeAndPod
		//==========================================================================

		for _, pnode := range projectedNodes {
			if !pnode.Committed {
				pnode1 = pnode
				break
			}
		}

		if pnode1 == nil {
			break
		}

		//==========================================================================
		// find a pnode2 you can merge pnode1 with
		//==========================================================================

		for pnode2IndexCandidate, pnode2Candidate := range projectedNodes {
			if pnode2Candidate == pnode1 { // don't want to merge with self
				continue
			}

			if pnode1.canMergeWith(pnode2Candidate) {
				pnode2 = pnode2Candidate
				pnode2Index = pnode2IndexCandidate
				break
			}
		}

		//==========================================================================
		// merge if found, OR scale down to the smallest instance size it can use and commit it
		//==========================================================================

		if pnode2 != nil {
			// Delete the partner being merged, and merge pods
			i := pnode2Index
			projectedNodes = append(projectedNodes[:i], projectedNodes[i+1:]...)
			pnode1.Pods = append(pnode1.Pods, pnode2.Pods...)
		} else {
			// If we can't merge with anyone, can we scale down to the lowest cost.
			// nodeSizes are asc. by cost, so the first we find is the cheapest.
			for _, nodeSize := range s.nodeSizes {
				if nodeSize.CPUCores >= pnode1.usedCPU() && nodeSize.RAMGIB >= pnode1.usedRAM() {
					pnode1.Size = nodeSize
					pnode1.Committed = true
					break
				}
			}
		}
	}

	// Load existing Nodes
	s.kube.Nodes = make([]*models.Node, 0)
	if err := s.core.DB.Preload("Kube.CloudAccount").Find(&s.kube.Nodes, "kube_id = ?", s.kube.ID); err != nil {
		return err
	}

	for _, node := range s.kube.Nodes {

		// TODO ---- need to label them to prevent disk overflow

		// eventual option to delete nodes when there are pods (w/ or wo/ volumes?) that could move to other nodes (we would have to calculate that)

		hasPods, err := s.core.Nodes.hasPodsWithReservedResources(node)
		if err != nil {
			return fmt.Errorf("Capacity service error when fetching Pods for Node: %s", err)
		}

		if !hasPods && time.Since(node.ProviderCreationTimestamp) > minAgeToExist {

			s.core.Log.Infof("Terminating node %s", node.Name)

			if err := s.core.Nodes.Delete(node.ID, node).Now(); err != nil {
				return fmt.Errorf("Capacity service error when deleting Node: %s", err)
			}
		}
	}

	// for _, pnode := range projectedNodes {
	// 	fmt.Println(pnode.Size.ID)
	// 	for _, pod := range pnode.Pods {
	// 		fmt.Println(pod.Metadata.Name)
	// 		for _, container := range pod.Spec.Containers {
	// 			if container.Resources != nil && container.Resources.Limits != nil {
	// 				fmt.Println(container.Resources.Limits.CPU)
	// 				fmt.Println(container.Resources.Limits.Memory)
	// 			}
	// 		}
	// 	}
	// 	fmt.Println("")
	// }

	for _, pnode := range projectedNodes {
		node := &models.Node{
			KubeID: s.kube.ID,
			Size:   pnode.Size.Name,
		}

		// If there's an existing node which is spinning up with this type, then
		// don't create.
		// This is a big TODO -- the logic should be much tighter, allowing ongoing
		// projection of pods onto nodes that are still spinning up.

		alreadySpinningUp := false
		for _, existingNode := range s.kube.Nodes {

			if existingNode.Size == node.Size && !existingNode.Ready {
				// This may be a node that is already being created, or NOTE it could
				// be a broken node that we erroneously identify as spinning up.
				alreadySpinningUp = true
				break
			}
		}
		if alreadySpinningUp {
			s.core.Log.Infof("Capacity service is already waiting on new node with size %s", node.Size)
			continue
		}

		s.core.Log.Infof("Capacity service is creating node with size %s", node.Size)

		if err := s.core.Nodes.Create(node); err != nil {
			return fmt.Errorf("Capacity service error when creating Node: %s", err)
		}
	}
	return nil
}

////////////////////////////////////////////////////////////////////////////////
//\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\
////////////////////////////////////////////////////////////////////////////////
//\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\
////////////////////////////////////////////////////////////////////////////////
//\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\
////////////////////////////////////////////////////////////////////////////////
//\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\
////////////////////////////////////////////////////////////////////////////////
//\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\

func (s *KubeScaler) hasTrackedEvent(pod *guber.Pod) (bool, error) {
	q := &guber.QueryParams{
		FieldSelector: "involvedObject.name=" + pod.Metadata.Name,
	}
	events, err := s.core.K8S(s.kube).Events("").Query(q)
	if err != nil {
		return false, err
	}

	for _, event := range events.Items {
		for _, message := range trackedEventMessages {
			if strings.Contains(event.Message, message) {
				return true, nil
			}
		}
	}
	return false, nil
}

func (s *KubeScaler) incomingPods() (incomingPods []*guber.Pod, err error) {
	waitStart := time.Now()

	for {
		incomingPods = incomingPods[:0] // reset

		q := &guber.QueryParams{
			FieldSelector: "status.phase=Pending",
		}
		pendingPods, err := s.core.K8S(s.kube).Pods("").Query(q)
		if err != nil {
			return nil, err
		}

		for _, pod := range pendingPods.Items {
			hasTrackedEvent, err := s.hasTrackedEvent(pod)
			if err != nil {
				return nil, err
			}
			if hasTrackedEvent {
				incomingPods = append(incomingPods, pod)
			}
		}

		elapsed := time.Since(waitStart)
		incomingCount := len(incomingPods)

		if incomingCount > 0 && elapsed < waitBeforeScale {
			s.core.Log.Infof("Waiting to add nodes for %d pods; %.1f seconds elapsed", incomingCount, elapsed.Seconds())

			for _, pod := range incomingPods {
				fmt.Println(pod.Metadata.Name)
			}

			time.Sleep(5 * time.Second)
		} else {
			break
		}
	}

	return incomingPods, nil
}

//------------------------------------------------------------------------------

type projectedNode struct {
	Committed bool
	Size      *NodeSize
	Pods      []*guber.Pod
}

func (pnode *projectedNode) usedRAM() (u float64) {
	for _, pod := range pnode.Pods {
		for _, container := range pod.Spec.Containers {

			// NOTE we use limits here, and not requests, because we want to spin up
			// nodes that are at least slightly bigger than the user thinks the pod
			// could utilize. This will ensure that the user's limit CAN BE FILLED AT
			// ALL. This is at the core of our increased-utilization strategy.

			var memStr string

			if container.Resources.Limits != nil {
				memStr = container.Resources.Limits.Memory
			} else if container.Resources.Requests != nil {
				memStr = container.Resources.Requests.Memory
			} else {
				continue
			}

			b := new(models.BytesValue)
			if err := b.UnmarshalJSON([]byte(memStr)); err != nil {
				panic(err)
			}

			u += b.Gibibytes()
		}
	}
	return
}

func (pnode *projectedNode) usedCPU() (u float64) {
	for _, pod := range pnode.Pods {
		for _, container := range pod.Spec.Containers {

			// NOTE above in usedRAM

			var cpuStr string

			if container.Resources.Limits != nil {
				cpuStr = container.Resources.Limits.CPU
			} else if container.Resources.Requests != nil {
				cpuStr = container.Resources.Requests.CPU
			} else {
				continue
			}

			c := new(models.CoresValue)
			if err := c.UnmarshalJSON([]byte(cpuStr)); err != nil {
				panic(err)
			}
			u += c.Cores()
		}
	}
	return
}

func (pnode *projectedNode) usedVolumes() (u int) {
	for _, pod := range pnode.Pods {
		for _, vol := range pod.Spec.Volumes {
			if vol.AwsElasticBlockStore != nil {
				u++
			}
		}
	}
	return
}

func (pnode1 *projectedNode) canMergeWith(pnode2 *projectedNode) bool {
	usedCPU := pnode1.usedCPU() + pnode2.usedCPU()
	usedRAM := pnode1.usedRAM() + pnode2.usedRAM()
	usedVolumes := pnode1.usedVolumes() + pnode2.usedVolumes()
	return pnode1.Size.CPUCores >= usedCPU && pnode1.Size.RAMGIB >= usedRAM && usedVolumes <= maxDisksPerNode
}
