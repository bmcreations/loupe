package mcpserver

import (
	"strings"
	"testing"
)

func TestCompactScreenDropsBareContainers(t *testing.T) {
	in := `{"target":"t","width":1200,"height":2670,"rotation":0,"nodes":[
	 {"class":"FrameLayout","id":"android:id/content","bounds":[0,0,1200,2670],"center":[600,1335],"children":[
	  {"class":"RecyclerView","id":"app:id/list","bounds":[0,0,1200,2000],"center":[600,1000],"scrollable":true,"scrollForward":true,"children":[
	   {"class":"ViewGroup","bounds":[0,0,1200,300],"center":[600,150],"clickable":true,"children":[
	    {"class":"TextView","text":"Severance","bounds":[0,0,600,100],"center":[300,50]}]}]},
	  {"class":"ImageView","id":"app:id/logo","bounds":[0,2000,100,2100],"center":[50,2050]},
	  {"class":"EditText","text":"Search","id":"app:id/q","bounds":[0,2100,1200,2200],"center":[600,2150],"focused":true,"password":false}]}]}`
	got := compactScreen([]byte(in))
	want := []string{
		"screen 1200x2670 rotation 0.",
		"Ids without a package are app;",
		"\nRecyclerView id=list @600,1000 scroll:fwd\n",
		"\n  ViewGroup @600,150 click\n",
		"\n    TextView \"Severance\" @300,50\n",
		"\nImageView id=logo @50,2050\n",
		"\nEditText \"Search\" id=q @600,2150 focused\n",
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
	if strings.Contains(got, "FrameLayout") {
		t.Errorf("bare container kept:\n%s", got)
	}
}

func TestCompactScreenPassesThroughWhatItCannotParse(t *testing.T) {
	for _, in := range []string{`not json`, `{"error":"noTree"}`} {
		if got := compactScreen([]byte(in)); got != in {
			t.Errorf("compactScreen(%q) = %q", in, got)
		}
	}
}
