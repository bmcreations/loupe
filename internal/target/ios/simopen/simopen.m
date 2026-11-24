// simopen opens an https URL in a named app on a simulator, the way iOS
// opens a universal link the app claims. Loupe runs it with
// `simctl spawn <udid> simopen <url> <bundleID>`.
//
// `simctl openurl` never checks universal links, so it gives every https
// URL to Safari. This hands the app an NSUserActivity of type
// NSUserActivityTypeBrowsingWeb instead, through LSApplicationWorkspace.
// A spawned process cannot ask swcd which app claims a URL (its XPC is
// invalidated), so loupe picks the app itself and names it here.
//
// FrontBoard refuses the open as "Request is not trusted" unless the
// binary carries com.apple.frontboard.launchapplications and
// com.apple.springboard.launchapplications, which loupe embeds in
// __TEXT,__entitlements when it builds this.
//
// The URL's path and query can carry a token, so nothing here prints the
// URL, or an NSError's description, which can quote it. A failure prints
// the error's domain and code only.
//
// Exit codes: 0 opened, 1 the open failed, 2 bad arguments, 3 no such
// app, 4 the open did not answer within 10 s.

#import <Foundation/Foundation.h>
#import <objc/message.h>
#include <stdio.h>

int main(int argc, char **argv) {
    @autoreleasepool {
        if (argc != 3) {
            fprintf(stderr, "usage: simopen <url> <bundleID>\n");
            return 2;
        }
        NSURL *url = [NSURL URLWithString:[NSString stringWithUTF8String:argv[1]]];
        NSString *bundle = [NSString stringWithUTF8String:argv[2]];
        if (!url) {
            fprintf(stderr, "simopen: not a URL\n");
            return 2;
        }
        Class recClass = NSClassFromString(@"LSApplicationRecord");
        Class wsClass = NSClassFromString(@"LSApplicationWorkspace");
        SEL recInit = NSSelectorFromString(@"initWithBundleIdentifier:allowPlaceholder:error:");
        SEL openSel = NSSelectorFromString(@"openUserActivity:usingApplicationRecord:configuration:completionHandler:");
        if (!recClass || !wsClass || ![wsClass instancesRespondToSelector:openSel]) {
            fprintf(stderr, "simopen: this runtime has no openUserActivity:usingApplicationRecord:\n");
            return 1;
        }
        NSError *err = nil;
        id rec = ((id (*)(id, SEL, NSString *, BOOL, NSError **))objc_msgSend)(
            [recClass alloc], recInit, bundle, NO, &err);
        if (!rec) {
            fprintf(stderr, "simopen: no app record: %s %ld\n",
                    err.domain.UTF8String ?: "?", (long)err.code);
            return 3;
        }
        NSUserActivity *activity = [[NSUserActivity alloc] initWithActivityType:NSUserActivityTypeBrowsingWeb];
        activity.webpageURL = url;
        id ws = ((id (*)(id, SEL))objc_msgSend)(wsClass, NSSelectorFromString(@"defaultWorkspace"));

        __block int rc = 1;
        dispatch_semaphore_t done = dispatch_semaphore_create(0);
        ((void (*)(id, SEL, id, id, id, id))objc_msgSend)(ws, openSel, activity, rec, nil, ^(BOOL ok, NSError *e) {
            if (!ok) {
                fprintf(stderr, "simopen: open failed: %s %ld\n",
                        e.domain.UTF8String ?: "?", (long)e.code);
            }
            rc = ok ? 0 : 1;
            dispatch_semaphore_signal(done);
        });
        if (dispatch_semaphore_wait(done, dispatch_time(DISPATCH_TIME_NOW, 10 * NSEC_PER_SEC)) != 0) {
            fprintf(stderr, "simopen: no answer in 10s\n");
            return 4;
        }
        return rc;
    }
}
