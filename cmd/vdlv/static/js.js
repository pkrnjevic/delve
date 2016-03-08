var running = false;
var quit = false;

function toggletop() {
	var half = document.getElementById("tophalf");
	var half2 = document.getElementById("bothalf");

	if (half.style.display == "none") {
		half.style.height = "calc(50% - 26px)";
		half.style.display = "block"
		half2.style.height = "calc(50% - 26px)";
	} else {
		half.style.display = "none";
		half2.style.height = "calc(100% - 52px)";
	}
	return false;
}

function ajaxit(url, fn) {
	var req = new XMLHttpRequest();
	req.onload = function() {
		fn(JSON.parse(this.responseText));
	}
	req.open("get", url);
	req.send(null);
}

var lastcmd = "";

function console_keyup(event) {
	if (event.which != 13) {
		return;
	}
	var c = document.getElementById('console');
	if (c.value.substring(c.selectionStart).trim() != "") {
		return;
	}

	var cmd = c.value;
	for (var i = c.selectionStart - 2; i >= 0; --i) {
		if (c.value[i] == '\n') {
			cmd = c.value.substring(i+1, c.selectionStart);
			break;
		}
	}

	cmd = cmd.trim();

	if (cmd.startsWith("(dlv)")) {
		cmd = cmd.substring(6).trim()
	}

	if (cmd == "") {
		if (!running && !quit) {
			cmd = lastcmd;
		} else {
			return;
		}
	}

	if (running) {
		alert("running");
		return;
	}
	if (quit) {
		alert("quit");
		return;
	}

	lastcmd = cmd;

	running = true;
	var ws = new WebSocket("ws://" + window.location.host + "/cmd?cmd=" + encodeURIComponent(cmd));
	ws.onmessage = function(evt) {
		var d = JSON.parse(evt.data);
		if (d.List != null) {
			reloadList(d.List.Filename, d.List.Line, d.List.ShowArrow)
		}
		quit = d.Quit
		c.value += d.Out;
		c.scrollTop = c.scrollHeight;
	};

	ws.onclose = function() {
		if (quit) {
			return;
		}
		c.value += "\n(dlv) ";
		c.scrollTop = c.scrollHeight;
		running = false;
	};
}

function reloadList(filename, line, showArrow) {
	ajaxit("/list?filename=" + encodeURIComponent(filename) + "&line=" + encodeURIComponent(line) + "&showArrow=" + encodeURIComponent(showArrow), function(data) {
		document.getElementById("tophalf").innerHTML = data.Out;
		var selines = document.getElementsByClassName("selectedline");
		if (selines.length > 0) {
			selin = selines[0];
			var ctxlin = document.getElementById("L" + (selin.id.substring(1) - 10));
			if (ctxlin != null) {
				ctxlin.scrollIntoView();
			}
			selin.scrollIntoViewIfNeeded();
		}

		for (var i = 0; i < data.Breakpoints.length; ++i) {
			var bp = data.Breakpoints[i];
			togglebreakpointIntl(bp);
		}
	})
}

function togglebreakpointIntl(bp) {
	var ln = document.getElementById("L" + bp.Line);
	var lnbp = ln.getElementsByClassName("linebp")[0];
	if (lnbp.innerHTML == "") {
		lnbp.innerHTML = bp.Name;
		var bpargs = document.createElement("div");
		bpargs.id = "bpargs" + bp.Name;
		bpargs.classList.add("line");
		var a = document.createElement("div");
		a.classList.add("linebp");
		bpargs.appendChild(a);
		a = document.createElement("div");
		a.classList.add("lineno");
		bpargs.appendChild(a);
		a = document.createElement("div");
		a.classList.add("linearr");
		bpargs.appendChild(a);
		a = document.createElement("textarea");
		a.cols = 30;
		a.rows = 2;
		a.classList.add("lineline");
		a.value = bp.Config;
		a.oninput = function() {
			breakpointchange(bp.Name);
		}
		bpargs.appendChild(a);
		ln.insertAdjacentElement('afterend', bpargs);
	}

	if (bp.Enabled) {
		lnbp.style.color = "black";
	} else {
		lnbp.style.color = "cyan";
	}

	document.getElementById("bpargs" + bp.Name).getElementsByTagName("TEXTAREA").value = bp.Config;
}

function togglebreakpoint(filename, line, contents, event) {
	if (running) {
		alert("running");
		return;
	}
	if (quit) {
		alert("quit");
		return;
	}
	
	var action = (event.button == 1) ? "delete" : "toggle";
	var url = "/bp?action=" + action + "&filename=" + encodeURIComponent(filename) + "&line=" + encodeURIComponent(line) + "&contents=" + encodeURIComponent(contents);
	if (event.button == 1) {
		ajaxit(url, function(bp) {
			document.getElementById("L" + bp.Line).getElementsByClassName("linebp")[0].innerHTML = "";
			var bpargs = document.getElementById("bpargs" + bp.Name);
			bpargs.parentNode.removeChild(bpargs);
		});
	} else {
		ajaxit(url, togglebreakpointIntl);
	}
	return false;
}

function breakpointchange(name) {
	if (running) {
		alert("running");
		return;
	}
	if (quit) {
		alert("quit");
		return;
	}
	var ta = document.getElementById("bpargs" + name).getElementsByTagName("TEXTAREA")[0];
	ta.rows = ta.value.split("\n").length;
	if (ta.rows < 2) ta.rows = 2;
	ajaxit("/bp?action=update&name=" + encodeURIComponent(name) + "&config=" + encodeURIComponent(ta.value), function(data) {
		if (!data.Ok) {
			ta.style['border'] = "1px solid red";
			return;
		}
		ta.style['border'] = "";
	});
}

function interrupt() {
	ajaxit("/interrupt", function(data) { /*nothing to do*/ });
}
