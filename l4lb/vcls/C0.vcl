vcl 4.1;

backend go_app {
    .host = "127.0.0.1";
    .port = "8080";
}

backend origin {
    .host = "192.168.88.30";
    .port = "8888";
}

sub vcl_recv {
    set req.http.X-NCDN-PoPCache-NodeId = "C0";
    if (req.url ~ "^/(latencyz|statusz)(/|$)") {
        set req.backend_hint = go_app;

        return (pass);
    }

    set req.backend_hint = origin;
}

sub vcl_backend_response {
    if (bereq.backend == origin &&
        beresp.status == 200) {
        set beresp.ttl = 30s;
        set beresp.grace = 30s;
    }
}

sub vcl_deliver {
    if (obj.hits > 0) {
        set resp.http.X-Cache = "HIT";
    } else {
        set resp.http.X-Cache = "MISS";
    }
}