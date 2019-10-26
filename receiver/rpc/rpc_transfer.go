package rpc

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	cmodel "github.com/open-falcon/common/model"
	cutils "github.com/open-falcon/common/utils"
	"github.com/open-falcon/transfer/g"
	"github.com/open-falcon/transfer/proc"
	"github.com/open-falcon/transfer/sender"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/push"
)

type Transfer int

type TransferResp struct {
	Msg        string
	Total      int
	ErrInvalid int
	Latency    int64
}

func (t *TransferResp) String() string {
	s := fmt.Sprintf("TransferResp total=%d, err_invalid=%d, latency=%dms",
		t.Total, t.ErrInvalid, t.Latency)
	if t.Msg != "" {
		s = fmt.Sprintf("%s, msg=%s", s, t.Msg)
	}
	return s
}

func (this *Transfer) Ping(req cmodel.NullRpcRequest, resp *cmodel.SimpleRpcResponse) error {
	return nil
}

func (t *Transfer) Update(args []*cmodel.MetricValue, reply *cmodel.TransferResponse) error {
	return RecvMetricValues(args, reply, "rpc")
}

// process new metric values
func RecvMetricValues(args []*cmodel.MetricValue, reply *cmodel.TransferResponse, from string) error {
	var ipAddr string // to store endpoint
	cfg := g.Config()
	if !cfg.Pushgateway.Enabled {
		return nil
	}
	pushgatewayURL := "http://" + cfg.Pushgateway.Listen

	start := time.Now()
	reply.Invalid = 0

	items := []*cmodel.MetaData{}

	// update: using global registery
	pusher := push.New(pushgatewayURL, "vm_monitor")

	// @@debug use:
	if g.Config().Debug {
		log.Println("[debug]: len of metrics in one rpc: ", len(args))
	}

	for _, v := range args {
		if v == nil {
			reply.Invalid += 1
			continue
		}

		// 历史遗留问题.
		// 老版本agent上报的metric=kernel.hostname的数据,其取值为string类型,现在已经不支持了;所以,这里硬编码过滤掉
		if v.Metric == "kernel.hostname" {
			reply.Invalid += 1
			continue
		}

		if v.Metric == "" || v.Endpoint == "" {
			reply.Invalid += 1
			continue
		}

		if v.Type != g.COUNTER && v.Type != g.GAUGE && v.Type != g.DERIVE {
			reply.Invalid += 1
			continue
		}

		if v.Value == "" {
			reply.Invalid += 1
			continue
		}

		if v.Step <= 0 {
			reply.Invalid += 1
			continue
		}

		if len(v.Metric)+len(v.Tags) > 510 {
			reply.Invalid += 1
			continue
		}

		// TODO 呵呵,这里需要再优雅一点
		now := start.Unix()
		if v.Timestamp <= 0 || v.Timestamp > now*2 {
			v.Timestamp = now
		}

		fv := &cmodel.MetaData{
			Metric:      v.Metric,
			Endpoint:    v.Endpoint,
			Timestamp:   v.Timestamp,
			Step:        v.Step,
			CounterType: v.Type,
			Tags:        cutils.DictedTagstring(v.Tags), //TODO tags键值对的个数,要做一下限制
		}

		valid := true
		var vv float64
		var err error

		switch cv := v.Value.(type) {
		case string:
			vv, err = strconv.ParseFloat(cv, 64)
			if err != nil {
				valid = false
			}
		case float64:
			vv = cv
		case int64:
			vv = float64(cv)
		default:
			valid = false
		}

		if !valid {
			reply.Invalid += 1
			continue
		}

		fv.Value = vv
		items = append(items, fv)

		// debug fv
		if g.Config().Debug {
			log.Println("[debug]: " + fv.String() + "\n")
		}

		// using go func to push to pushgateway(from cfg.json)
		// prefix: vm_
		// strings.Join(strings.Split(msg, "."), "_")
		// fund bug

		metricName := strings.Join(strings.Split(fv.Metric, "."), "_")
		metricName = strings.Join(strings.Split(metricName, "-"), "_")
		metricName = "vm_monitor_" + metricName

		// using endpoint to ipAddr
		// @@ 优化：string copy using time
		ipAddr = fv.Endpoint
		if fv.CounterType == "GAUGE" {
			if g.Config().Debug {
				log.Println("[debug]: " + metricName)
			}
			metricProme := prometheus.NewGauge(prometheus.GaugeOpts{
				Name:        metricName,
				ConstLabels: fv.Tags,
			})
			// registry := prometheus.NewRegistry()
			// registry.MustRegister(metricProme)
			metricProme.Set(float64(fv.Value))
			// pusher := push.New("http://10.10.26.24:9091", "vm_monitor")

			// add metrics
			pusher.Collector(metricProme)
			// if err := pusher.Push(); err != nil {
			// 	fmt.Println("Could not push to Pushgateway:", err)
			// }
		} else { // counter
			if g.Config().Debug {
				log.Println("[debug]: " + metricName)
			}
			metricProme := prometheus.NewCounter(prometheus.CounterOpts{
				Name:        metricName,
				ConstLabels: fv.Tags,
			})
			// registry := prometheus.NewRegistry()
			// registry.MustRegister(metricProme)
			metricProme.Add(float64(fv.Value))
			// pusher := push.New("http://10.10.26.24:9091", "vm_monitor")

			// add metrics
			pusher.Collector(metricProme)
			// if err := pusher.Push(); err != nil {
			// 	fmt.Println("Could not push to Pushgateway:", err)
			// }
		}
	} // end for

	// send total metrics at once
	// @@change Push() to Add()
	// async add
	if err := pusher.Grouping("instance", ipAddr).Add(); err != nil {
		fmt.Println("Could not push to Pushgateway:", err)
	}

	// statistics
	cnt := int64(len(items))
	proc.RecvCnt.IncrBy(cnt)
	if from == "rpc" {
		proc.RpcRecvCnt.IncrBy(cnt)
	} else if from == "http" {
		proc.HttpRecvCnt.IncrBy(cnt)
	}

	if cfg.Graph.Enabled {
		sender.Push2GraphSendQueue(items)
	}

	if cfg.Judge.Enabled {
		sender.Push2JudgeSendQueue(items)
	}

	if cfg.Tsdb.Enabled {
		sender.Push2TsdbSendQueue(items)
	}

	reply.Message = "ok"
	reply.Total = len(args)
	reply.Latency = (time.Now().UnixNano() - start.UnixNano()) / 1000000

	return nil
}
