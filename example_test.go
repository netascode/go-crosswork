package crosswork_test

import (
	"context"

	"github.com/netascode/go-crosswork"
)

// These examples mirror the README. They have no Output comment, so they are compiled but not run.

func ExampleNewClient() {
	client, _ := crosswork.NewClient("https://1.1.1.1:30603", "user", "pwd")

	res, _ := client.Get("/crosswork/inventory/v1/nodes/count")
	println(res.Get("number_of_nodes").String())
}

func ExampleClient_Post() {
	client, _ := crosswork.NewClient("https://1.1.1.1:30603", "user", "pwd")

	res, _ := client.Post("/crosswork/inventory/v1/nodes/query", `{"limit":10}`)
	for _, node := range res.Get("data").Array() {
		println(node.Get("host_name").String())
	}

	body := crosswork.Body{}.
		Set("data.0.profile", "example").
		Set("data.0.user_pass.0.user_name", "admin").
		Set("data.0.user_pass.0.password", "secret").
		Set("data.0.user_pass.0.type", "ROBOT_USERPASS_SSH")
	client.Post("/crosswork/inventory/v1/credentials", body.Str, crosswork.NoLogPayload)
}

func ExampleClient_DeleteBody() {
	client, _ := crosswork.NewClient("https://1.1.1.1:30603", "user", "pwd")
	_, err := client.DeleteBody("/crosswork/inventory/v1/credentials", `{"data":[{"profile":"example"}]}`)
	if crosswork.IsNotFound(err) {
		println("gone")
	}
}

func ExampleWithToken() {
	client, _ := crosswork.NewClient("https://1.1.1.1:30603", "", "", crosswork.WithToken("mytoken"))
	_, _ = client.Get("/crosswork/sso/v2/userInfo")
}

func ExampleClient_WithContext() {
	client, _ := crosswork.NewClient("https://1.1.1.1:30603", "user", "pwd")
	var ctx context.Context = context.Background()

	c := client.WithContext(ctx) // assign first: the methods have pointer receivers
	_, _ = c.Get("/crosswork/inventory/v1/nodes/count")
}

func ExampleInsecure() {
	_, _ = crosswork.NewClient("https://1.1.1.1:30603", "user", "pwd", crosswork.Insecure(false))
}

func ExampleClient_YangPatch() {
	client, _ := crosswork.NewClient("https://1.1.1.1:30603", "user", "pwd", crosswork.NSORetry(120))

	_, _ = client.YangPatch("/crosswork/proxy/nso/restconf/data", "patch-1", "", []crosswork.YangPatchEdit{
		crosswork.NewYangPatchEdit("merge", "/tailf-ncs:devices/authgroups/group=g1",
			crosswork.Body{}.SetRaw("tailf-ncs:group", `[{"name":"g1"}]`)),
		crosswork.NewYangPatchEdit("remove", "/tailf-ncs:devices/authgroups/group=g2", crosswork.Body{}),
	})
}

func ExampleClient_Paginate() {
	client, _ := crosswork.NewClient("https://1.1.1.1:30603", "user", "pwd")

	nodes, _ := client.Paginate(crosswork.NewCursorPager("/crosswork/inventory/v1/nodes/query", "", 100))
	println(len(nodes))

	_, _ = client.Paginate(&crosswork.TokenPager{Path: "/crosswork/collection/v1/collectionjob/summary/query", PageSize: 100, ItemsPath: "collection_job_status_list"})
	_, _ = client.Paginate(&crosswork.PageNumberPager{Path: "/crosswork/performance/v1/policies/inventory-devices", PageParam: "page", SizeParam: "pageSize", FirstPage: 1, Size: 100, ItemsPath: "data"})
}
