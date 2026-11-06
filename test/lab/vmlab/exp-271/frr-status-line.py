import json
d = json.load(open("/tmp/srfrr_status.json"))
def g(k):
    return d[k] if k in d else "<旧版无此字段>"
print("injected=%s reported=%s dropped_loop=%s withheld=%s frozen=%s frozen_held=%s zebra=%s last_err=%r" % (
    d.get("injected"), d.get("reported"), d.get("dropped_loop"),
    g("local_withheld"), g("export_frozen"), g("export_frozen_held"),
    d.get("zebra_connected"), d.get("last_error")))
