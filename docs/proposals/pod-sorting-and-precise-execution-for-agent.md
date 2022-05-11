# Pod Sorting And Precise Execution For Crane Agent
该proposal能够在保证节点qos的前提下，尽可能减少对当前节点上业务的影响范围；在executor阶段，从cpu，memory等多维度去排序需要被执行操作的pod，而不仅仅是依赖ProrityClass；另外会依照实时用量和水位线的差距以及执行操作pod所释放出来的资源用量进行细粒度的驱逐或压制操作，减少过度多余的无谓操作。

## Table of Contents

<!-- TOC -->

- [Pod Sorting And Precise Execution For Crane Agent](#Pod Sorting And Precise Execution For Crane Agent)
    - [Table of Contents](#table-of-contents)
    - [Motivation](#motivation)
        - [Goals](#goals)
        - [Non-Goals/Future Work](#non-goalsfuture-work)
    - [Proposal](#proposal)
        - [支持多维数据排序](#支持多维数据排序)
        - [以水位线为基准进行pod的操作](#以水位线为基准进行pod的操作)
        - [User Stories](#user-stories)
            - [Story 1](#story-1)
            - [Story 2](#story-2)
        - [Risks and Mitigations](#risks-and-mitigations)

<!-- /TOC -->
## Motivation
当前在crane agent中，当超过NodeQOSEnsurancePolicy中指定的水位线后，执行evict，throttle等操作时会对所有的pod进行排序，当前排序的依据是pod所属的ProrityClass，之后对节点上的所有pod进行throttle或者evict； 
存在的问题有：
1. 排序的方式是比较简单的，无法满足根据其他特性进行排序
2. 多节点所有pod进行操作会造成过度操作，如果能以水位线为基准进行操作是更为合适的

### Goals

- 实现多种指标的排序，比如以cpu使用为主要参照的排序
- 执行动作之前以水位线为基准，优先不可压缩资源，其次是可压缩资源


### Non-Goals/Future Work

- 暂时不支持自定义排序策略
- 当前只支持CPU和memory维度的细粒度操作

## Proposal
### 支持多维数据排序
以CPU排序为例，一次比较两个pod的classAndPriority, cpuUsage, extCpuUsage, runningTime，当某一个指标存在差异时即返回

同时用指标名同指标的排序方法<metric>SortPod进行关联

同时，为了应对将来添加的多种维度指标，创建一个默认的排序方法构造函数，只需要传入对应的metric name，同时实现<metric>-sort-func即可
```go
func General<Metric>Sorter(pods []podinfo.PodContext, <metric> string) {
    orderedBy(classAndPriority, <metric>-sort-func, runningTime).Sort(pods)
}
```

### 以水位线为基准进行pod的操作
analyzer阶段：
首先会通过多个NEP及其中的objectiveEnsurances构建多条水位线，并按照NEP对应的action进行分类，之后对同一类中的水位线进行再按照metric rule进行分类和排序；
最终形成一个类似下图的数据存储：
![](waterline-construct.png)

同时按照当前用量与水位线中最小值的差值构造如下的数据结构，需要注意对于ThrottleUp，需要用水位线最小值-当前用量作为gap值，对于其他两者，使用当前用量-水位线最小值作为gap值
```go
EvictGapToWaterLines[metrics]
ThrottoleDownGapToWaterLines[metrics]
ThrottleUpGapWaterLine[metrics]
```

在这些不同metric rule构造的水位线中，有的metric是可以量化的，有的是不可以量化的，这在之后的实际执行阶段会有一定的作用，这么区分的原因是对于不可量化的指标，我们无法评估压制或者驱逐一个pod
到底会释放多少资源，无法精确计算离水位线的差距，比如，压制一个pod的cpu，压制后释放的cpu用量我们可以评估，但很多指标对pod进行操作后对水位线的影响无法评估；

executor阶段：
本次的proposal只涉及throttle和evict动作
1.首先判断水位线中是否存在不可量化的指标，如果存在，那么对所有的pod进行操作，因为我们无法知道操作一个pod会释放多少的该资源，也就无法准确评估到水位线的距离；
2.如果不存在不可量化指标，则在执行前再获取一次所有水位线（上文的EvictGapToWaterLines，ThrottleDownWaterLine，ThrottleUpWaterLine）中涉及的metrics的节点的最新使用量，如果有获取不到metric用量的，因为缺失造成无法预估资源释放多少合适，所以将对所有pod进行操作；
3.针对每种指标，需要实现Throttle<metric>和Construct<metric>Release方法，用指标名进行关联，同时针对evict和throttle，维护一个数组，保存进行操作的指标的顺序，以不可压缩资源->压缩资源为顺序，如：
throttleMetricList:[memory, cpu, metricA, metricB], evictMetricList:[memory, cpu, metricA, metricB]；
针对压制：
按照throttleMetricList中的指标对应的排序方式对pod进行排序后，在没有达到对应metric的水位线前，按顺序取出一个pod，执行压制操作，并计算释放的该metric对应的资源，同时在水位线中减去该释放的值，直到满足水位线要求
针对驱逐：
按照EvictGapToWaterLines中的指标对应的排序方式对pod进行排序后，在没有达到对应metric的水位线前，取出一个没有执行过的pod，执行驱逐操作，并计算释放出的各metric资源，同时在对应水位线中减去释放的值，直到满足当前metric水位线要求
```go

GetLatestUsage() //获取最新的指标用量，用于保证在执行操作前的数据时效性
do throttle:
    for <metric> in throttleMetricList{
		<metric>SortPod() //按照metric的排序方式对pod进行排序
        podIndex:=0
        for !ThrottoleDownGapToWaterLines[<metric>] reached{
            pod=SortedPodList[podindex]
            Throttle<metric>(pod)
            realsed<metric>resource:= Construct<metric>Release(pod)
            ThrottoleDownGapToWaterLines[<metric>]-=realsed<metric>resource
            podindex++
        }
    }
	
do evict:
    for <metric> in evictMetricList{
        <metric>SortPod() //按照metric的排序方式对pod进行排序
        for !EvictGapToWaterLines[<metric>] reached{
            if Find pod NoExecuted {
                Evict(pod)
                markAsExecuted(pod)
                for <metric> in evictMetricList{
                    realsed<metric>resource:= Construct<metric>Release(pod)
                    EvictGapToWaterLines[<metric>]-=realsed<metric>resource
                }
            }
        }
    }

``` 


### User Stories

- 用户可以更为精确地使用agent进行qos的保障，避免影响过多的pod，提升业务的稳定性

#### Story 1
  定制自己希望的pod排序方式
#### Story 2
  按照水位线去准确调控业务，满足节点qos要求的情况下，尽可能保证操作的影响范围足够小

### Risks and Mitigations

