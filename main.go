package main

import (
	"context"
	"log"
	"math/rand"
	"os"
	"time"

	"github.com/jessevdk/go-flags"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
)

var params struct {
	Connect          string  `short:"c" long:"connect" description:"connect to lnd using host:port" default:"127.0.0.1:10009"`
	TLSCert          string  `short:"t" long:"tlscert" description:"path to tls.cert to connect" default:"tls.cert"`
	MacaroonDir      string  `long:"macaroon.dir" description:"path to the macaroon directory" required:"false"`
	MacaroonFilename string  `long:"macaroon.filename" description:"macaroon filename" default:"admin.macaroon"`
	Network          string  `short:"n" long:"network" description:"bitcoin network to use" default:"mainnet"`
	FromPerc         int64   `long:"pfrom" description:"channels with less than this inbound liquidity percentage will be considered as source channels" default:"50"`
	ToPerc           int64   `long:"pto" description:"channels with less than this outbound liquidity percentage will be considered as target channels" default:"50"`
	Perc             int64   `short:"p" long:"perc" description:"use this value as both pfrom and pto from above" default:"0"`
	Amount           int64   `short:"a" long:"amount" description:"amount to rebalance" default:"0"`
	EconRatio        float64 `long:"econ-ratio" description:"economical ratio for fee limit calculation as a multiple of target channel fee (for example, 0.5 means you want to pay at max half the fee you might earn for routing out of the target channel)" default:"1"`
}

type regolancer struct {
	lnClient     lnrpc.LightningClient
	routerClient routerrpc.RouterClient
	myPK         string
	channels     []*lnrpc.Channel
	fromChannels []*lnrpc.Channel
	toChannels   []*lnrpc.Channel
	nodeCache    map[string]*lnrpc.NodeInfo
	chanCache    map[uint64]*lnrpc.ChannelEdge
}

func main() {
	_, err := flags.Parse(&params)
	if err != nil {
		os.Exit(1)
	}
	rand.Seed(time.Now().UnixNano())
	if params.Perc > 0 {
		params.FromPerc = params.Perc
		params.ToPerc = params.Perc
	}
	conn, err := lndclient.NewBasicConn(params.Connect, params.TLSCert, params.MacaroonDir, params.Network,
		lndclient.MacFilename(params.MacaroonFilename))
	if err != nil {
		log.Fatal(err)
	}
	r := regolancer{nodeCache: map[string]*lnrpc.NodeInfo{}, chanCache: map[uint64]*lnrpc.ChannelEdge{}}
	r.lnClient = lnrpc.NewLightningClient(conn)
	r.routerClient = routerrpc.NewRouterClient(conn)
	info, err := r.lnClient.GetInfo(context.Background(), &lnrpc.GetInfoRequest{})
	if err != nil {
		log.Fatal(err)
	}
	r.myPK = info.IdentityPubkey
	err = r.getChannels()
	if err != nil {
		log.Fatal("Error listing own channels: ", err)
	}
	err = r.getChannelCandidates(params.FromPerc, params.ToPerc, params.Amount)
	if err != nil {
		log.Fatal("Error choosing channels: ", err)
	}
	if len(r.fromChannels) == 0 {
		log.Fatal("No source channels selected")
	}
	if len(r.toChannels) == 0 {
		log.Fatal("No target channels selected")
	}
	var invoice *lnrpc.AddInvoiceResponse
	attempt := 1
	for {
		from, to, amt := r.pickChannelPair(params.Amount)
		if params.Amount == 0 || invoice == nil {
			invoice, err = r.createInvoice(from, to, amt)
			if err != nil {
				log.Fatal("Error creating invoice: ", err)
			}
		}
		routes, err := r.getRoutes(from, to, amt*1000, params.EconRatio)
		if err != nil {
			continue
		}
		for _, route := range routes {
			log.Printf("Attempt %s, amount: %s", hiWhiteColor("#%d", attempt), hiWhiteColor("%d", amt))
			r.printRoute(route)
			err = r.pay(invoice, amt, route)
			if err == nil {
				log.Print("Task complete.")
				return
			}
			attempt++
		}
	}
}