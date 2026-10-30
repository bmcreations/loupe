// loupecam gives an iOS simulator app a back camera that sees an image file.
//
// The simulator has no camera: AVCaptureDevice answers nil for every device
// type. Loupe launches an app with this library in DYLD_INSERT_LIBRARIES and
// the image's host path in LOUPE_CAMERA_FILE, and it stands in for the parts
// of AVFoundation a scanner touches:
//
//   - AVCaptureDevice's lookups answer one fake back camera, and camera
//     access reads as authorized.
//   - AVCaptureDeviceInput wraps it, and AVCaptureSession keeps its inputs,
//     outputs and running state itself, never reaching the real capture
//     stack.
//   - While a session runs, every AVCaptureVideoDataOutput's delegate gets the
//     image 30 times a second, in the pixel format its videoSettings ask for,
//     420v when they ask for none, as on a phone.
//   - AVCaptureMetadataOutput offers QR, and reports what CIDetector finds in
//     the image.
//   - AVCaptureVideoPreviewLayer shows the image in a sublayer, with its
//     videoGravity.
//
// The file is polled four times a second, so replacing it changes what the
// camera sees without relaunching. A missing file is a black frame.
//
// It is built by loupe with `xcrun clang`, without ARC: the stand-ins are
// made with alloc and no init, because AVFoundation's initializers reach for
// hardware, and they live as long as the process.

#import <AVFoundation/AVFoundation.h>
#import <CoreImage/CoreImage.h>
#import <ImageIO/ImageIO.h>
#import <QuartzCore/QuartzCore.h>
#import <objc/runtime.h>
#import <sys/stat.h>

#define LCLog(fmt, ...) NSLog(@"loupecam: " fmt, ##__VA_ARGS__)

static void LCSwap(Class c, SEL orig, SEL repl) {
	Method m1 = class_getInstanceMethod(c, orig);
	Method m2 = class_getInstanceMethod(c, repl);
	if (!m1 || !m2) {
		LCLog(@"%@ has no %@", NSStringFromClass(c), NSStringFromSelector(orig));
		return;
	}
	// Added to c itself when c only inherits it, so the swap stays on c.
	if (class_addMethod(c, orig, method_getImplementation(m2), method_getTypeEncoding(m2))) {
		class_replaceMethod(c, repl, method_getImplementation(m1), method_getTypeEncoding(m1));
	} else {
		method_exchangeImplementations(m1, m2);
	}
}

static void LCSwapClass(Class c, SEL orig, SEL repl) { LCSwap(object_getClass(c), orig, repl); }

#pragma mark - Frame source

// LCFrame is one decoded image and everything made from it, rebuilt when the
// file changes. Buffers are made once per format and handed to every frame:
// a delegate only reads them.
@interface LCFrame : NSObject {
@public
	CGImageRef image; // NULL when there is no file
	size_t width, height;
	NSMutableDictionary *buffers; // @(OSType) -> CVPixelBufferRef
	NSArray *codes;               // LCCode
	NSUInteger generation;
}
- (CVPixelBufferRef)bufferForFormat:(OSType)format angle:(int)angle;
@end

// The camera's native buffer is landscape, kLCLong x kLCShort, as a phone's
// back camera delivers it with a connection rotation of 0. The image is held
// upright in front of a phone in portrait, so it fills a kLCShort x kLCLong
// canvas.
enum { kLCLong = 1920, kLCShort = 1080 };

// LCPath is the image file. A relative path is under the app's home
// directory, which is where a physical device's copy has to live.
static NSString *LCPath(void) {
	NSString *p = [[[NSProcessInfo processInfo] environment] objectForKey:@"LOUPE_CAMERA_FILE"];
	if (p.length && !p.isAbsolutePath) {
		p = [NSHomeDirectory() stringByAppendingPathComponent:p];
	}
	return p;
}

// LCCanvas fits image whole into a w x h canvas, centred on white, so a code
// held up to the camera is never cropped. The emulator's imagefile camera
// crops to fill instead.
static CGImageRef LCCanvas(CGImageRef image, size_t w, size_t h) {
	CGColorSpaceRef cs = CGColorSpaceCreateWithName(kCGColorSpaceSRGB);
	CGContextRef ctx = CGBitmapContextCreate(NULL, w, h, 8, 0, cs, kCGImageAlphaPremultipliedFirst | kCGBitmapByteOrder32Little);
	CGColorSpaceRelease(cs);
	// White under a transparent image, the way it would look on a screen.
	CGContextSetRGBFillColor(ctx, 1, 1, 1, 1);
	CGContextFillRect(ctx, CGRectMake(0, 0, w, h));
	CGFloat iw = CGImageGetWidth(image), ih = CGImageGetHeight(image);
	CGFloat scale = MIN(w / iw, h / ih);
	CGContextSetInterpolationQuality(ctx, kCGInterpolationHigh);
	CGContextDrawImage(ctx, CGRectMake((w - iw * scale) / 2, (h - ih * scale) / 2, iw * scale, ih * scale), image);
	CGImageRef out = CGBitmapContextCreateImage(ctx);
	CGContextRelease(ctx);
	return out;
}

// LCBufferSize is the size of a buffer delivered at a connection rotation:
// the native landscape buffer, turned by angle degrees clockwise.
static void LCBufferSize(int angle, size_t *w, size_t *h) {
	BOOL portrait = angle == 90 || angle == 270;
	*w = portrait ? kLCShort : kLCLong;
	*h = portrait ? kLCLong : kLCShort;
}

// LCCreateBGRA renders the upright canvas as a buffer at a connection
// rotation. The native buffer is the upright scene turned 90 degrees
// counterclockwise, so the scene is turned by angle - 90 clockwise.
static CVPixelBufferRef LCCreateBGRA(CGImageRef image, int angle) {
	size_t w, h;
	LCBufferSize(angle, &w, &h);
	NSDictionary *attrs = @{(id)kCVPixelBufferIOSurfacePropertiesKey : @{}};
	CVPixelBufferRef pb = NULL;
	if (CVPixelBufferCreate(NULL, w, h, kCVPixelFormatType_32BGRA, (CFDictionaryRef)attrs, &pb) != kCVReturnSuccess) {
		return NULL;
	}
	CVPixelBufferLockBaseAddress(pb, 0);
	CGColorSpaceRef cs = CGColorSpaceCreateWithName(kCGColorSpaceSRGB);
	CGContextRef ctx = CGBitmapContextCreate(CVPixelBufferGetBaseAddress(pb), w, h, 8, CVPixelBufferGetBytesPerRow(pb), cs,
	                                         kCGImageAlphaPremultipliedFirst | kCGBitmapByteOrder32Little);
	// Black with no image: a covered lens.
	CGContextSetRGBFillColor(ctx, 0, 0, 0, 1);
	CGContextFillRect(ctx, CGRectMake(0, 0, w, h));
	if (image) {
		// Core Graphics turns counterclockwise for a positive angle, y up.
		CGContextTranslateCTM(ctx, w / 2.0, h / 2.0);
		CGContextRotateCTM(ctx, -(angle - 90) * M_PI / 180);
		CGFloat iw = CGImageGetWidth(image), ih = CGImageGetHeight(image);
		CGContextDrawImage(ctx, CGRectMake(-iw / 2, -ih / 2, iw, ih), image);
	}
	CGContextRelease(ctx);
	CGColorSpaceRelease(cs);
	CVPixelBufferUnlockBaseAddress(pb, 0);
	return pb;
}

static inline uint8_t LCClamp(int v) { return v < 0 ? 0 : (v > 255 ? 255 : (uint8_t)v); }

// LCCreateNV12 converts BGRA to biplanar 4:2:0 with BT.601 coefficients, in
// video range for 420v and full range for 420f. Width and height are even.
static CVPixelBufferRef LCCreateNV12(CVPixelBufferRef bgra, OSType format) {
	size_t w = CVPixelBufferGetWidth(bgra), h = CVPixelBufferGetHeight(bgra);
	NSDictionary *attrs = @{(id)kCVPixelBufferIOSurfacePropertiesKey : @{}};
	CVPixelBufferRef pb = NULL;
	if (CVPixelBufferCreate(NULL, w, h, format, (CFDictionaryRef)attrs, &pb) != kCVReturnSuccess) {
		return NULL;
	}
	BOOL full = format == kCVPixelFormatType_420YpCbCr8BiPlanarFullRange;
	CVPixelBufferLockBaseAddress(bgra, kCVPixelBufferLock_ReadOnly);
	CVPixelBufferLockBaseAddress(pb, 0);
	const uint8_t *src = CVPixelBufferGetBaseAddress(bgra);
	size_t srs = CVPixelBufferGetBytesPerRow(bgra);
	uint8_t *yp = CVPixelBufferGetBaseAddressOfPlane(pb, 0);
	size_t yrs = CVPixelBufferGetBytesPerRowOfPlane(pb, 0);
	uint8_t *cp = CVPixelBufferGetBaseAddressOfPlane(pb, 1);
	size_t crs = CVPixelBufferGetBytesPerRowOfPlane(pb, 1);
	for (size_t y = 0; y < h; y += 2) {
		for (size_t x = 0; x < w; x += 2) {
			int rs = 0, gs = 0, bs = 0;
			for (int dy = 0; dy < 2; dy++) {
				for (int dx = 0; dx < 2; dx++) {
					const uint8_t *p = src + (y + dy) * srs + (x + dx) * 4;
					int b = p[0], g = p[1], r = p[2];
					rs += r, gs += g, bs += b;
					int luma = full ? (77 * r + 150 * g + 29 * b + 128) >> 8 : 16 + ((66 * r + 129 * g + 25 * b + 128) >> 8);
					yp[(y + dy) * yrs + x + dx] = LCClamp(luma);
				}
			}
			int r = rs / 4, g = gs / 4, b = bs / 4;
			int cb, cr;
			if (full) {
				cb = 128 + ((-43 * r - 85 * g + 128 * b + 128) >> 8);
				cr = 128 + ((128 * r - 107 * g - 21 * b + 128) >> 8);
			} else {
				cb = 128 + ((-38 * r - 74 * g + 112 * b + 128) >> 8);
				cr = 128 + ((112 * r - 94 * g - 18 * b + 128) >> 8);
			}
			cp[(y / 2) * crs + x] = LCClamp(cb);
			cp[(y / 2) * crs + x + 1] = LCClamp(cr);
		}
	}
	CVPixelBufferUnlockBaseAddress(pb, 0);
	CVPixelBufferUnlockBaseAddress(bgra, kCVPixelBufferLock_ReadOnly);
	CVBufferSetAttachment(pb, kCVImageBufferYCbCrMatrixKey, kCVImageBufferYCbCrMatrix_ITU_R_601_4, kCVAttachmentMode_ShouldPropagate);
	CVBufferSetAttachment(pb, kCVImageBufferColorPrimariesKey, kCVImageBufferColorPrimaries_ITU_R_709_2, kCVAttachmentMode_ShouldPropagate);
	CVBufferSetAttachment(pb, kCVImageBufferTransferFunctionKey, kCVImageBufferTransferFunction_ITU_R_709_2, kCVAttachmentMode_ShouldPropagate);
	return pb;
}

#pragma mark - Metadata

@interface LCCode : AVMetadataMachineReadableCodeObject {
@public
	NSString *lcString;
	CGRect lcBounds;
	NSArray *lcCorners;
}
@end

@implementation LCCode
- (AVMetadataObjectType)type { return AVMetadataObjectTypeQRCode; }
- (NSString *)stringValue { return lcString; }
- (CGRect)bounds { return lcBounds; }
- (NSArray *)corners { return lcCorners; }
- (CMTime)time { return CMClockGetTime(CMClockGetHostTimeClock()); }
- (CMTime)duration { return kCMTimeInvalid; }
- (CIBarcodeDescriptor *)descriptor { return nil; }
- (NSInteger)groupID { return 0; }
- (NSInteger)objectID { return 0; }
@end

// LCFindCodes reads QR codes out of the upright canvas. AVFoundation reports
// bounds and corners normalized to the native landscape buffer, origin top
// left, whatever the connection's rotation, so an upright point (u, v) is
// reported as (v, 1 - u).
static NSArray *LCFindCodes(CGImageRef image, size_t w, size_t h) {
	if (!image) {
		return @[];
	}
	CIDetector *d = [CIDetector detectorOfType:CIDetectorTypeQRCode context:nil options:@{CIDetectorAccuracy : CIDetectorAccuracyHigh}];
	NSArray *features = [d featuresInImage:[CIImage imageWithCGImage:image]];
	NSMutableArray *out = [NSMutableArray array];
	CGFloat W = (CGFloat)CGImageGetWidth(image), H = (CGFloat)CGImageGetHeight(image);
	for (CIFeature *f in features) {
		if (![f isKindOfClass:[CIQRCodeFeature class]]) {
			continue;
		}
		CIQRCodeFeature *q = (CIQRCodeFeature *)f;
		LCCode *c = [LCCode alloc];
		c->lcString = [q.messageString copy];
		CGRect b = q.bounds;
		// Upright, origin top left, then turned into the native buffer.
		CGRect u = CGRectMake(b.origin.x / W, 1 - (b.origin.y + b.size.height) / H, b.size.width / W, b.size.height / H);
		c->lcBounds = CGRectMake(u.origin.y, 1 - (u.origin.x + u.size.width), u.size.height, u.size.width);
		NSMutableArray *corners = [NSMutableArray array];
		CGPoint pts[4] = {q.topLeft, q.topRight, q.bottomRight, q.bottomLeft};
		for (int i = 0; i < 4; i++) {
			CGPoint p = pts[i];
			CGFloat u = p.x / W, v = 1 - p.y / H;
			CFDictionaryRef dict = CGPointCreateDictionaryRepresentation(CGPointMake(v, 1 - u));
			[corners addObject:(id)dict];
			CFRelease(dict);
		}
		c->lcCorners = [corners copy];
		[out addObject:c];
		[c release];
	}
	return out;
}

@implementation LCFrame
- (instancetype)initWithPath:(NSString *)path generation:(NSUInteger)gen {
	if ((self = [super init])) {
		buffers = [NSMutableDictionary new];
		generation = gen;
		CGImageSourceRef src = path ? CGImageSourceCreateWithURL((CFURLRef)[NSURL fileURLWithPath:path], NULL) : NULL;
		if (src) {
			// The thumbnail call applies EXIF orientation and caps the size at
			// a 1080p camera's long edge; it does not scale a small image up.
			NSDictionary *opts = @{
				(id)kCGImageSourceCreateThumbnailFromImageAlways : @YES,
				(id)kCGImageSourceCreateThumbnailWithTransform : @YES,
				(id)kCGImageSourceThumbnailMaxPixelSize : @1920,
			};
			CGImageRef decoded = CGImageSourceCreateThumbnailAtIndex(src, 0, (CFDictionaryRef)opts);
			CFRelease(src);
			if (decoded) {
				image = LCCanvas(decoded, kLCShort, kLCLong);
				CGImageRelease(decoded);
			}
		}
		width = kLCShort, height = kLCLong;
		codes = [LCFindCodes(image, width, height) retain];
	}
	return self;
}
- (CVPixelBufferRef)bufferForFormat:(OSType)format angle:(int)angle {
	@synchronized(self) {
		NSString *key = [NSString stringWithFormat:@"%u/%d", (unsigned)format, angle];
		id have = [buffers objectForKey:key];
		if (have) {
			return (CVPixelBufferRef)have;
		}
		NSString *bgraKey = [NSString stringWithFormat:@"%u/%d", (unsigned)kCVPixelFormatType_32BGRA, angle];
		CVPixelBufferRef bgra = (CVPixelBufferRef)[buffers objectForKey:bgraKey];
		if (!bgra) {
			bgra = LCCreateBGRA(image, angle);
			if (!bgra) {
				return NULL;
			}
			[buffers setObject:(id)bgra forKey:bgraKey];
			CVPixelBufferRelease(bgra);
		}
		if (format == kCVPixelFormatType_32BGRA) {
			return bgra;
		}
		CVPixelBufferRef pb = LCCreateNV12(bgra, format);
		if (!pb) {
			return NULL;
		}
		[buffers setObject:(id)pb forKey:key];
		CVPixelBufferRelease(pb);
		return pb;
	}
}
- (void)dealloc {
	CGImageRelease(image);
	[buffers release];
	[codes release];
	[super dealloc];
}
@end

static LCFrame *gFrame;              // guarded by gFrameLock
static NSLock *gFrameLock;
static struct timespec gFileTime;
static off_t gFileSize = -1;
static NSUInteger gGeneration;

static LCFrame *LCCurrentFrame(void) {
	[gFrameLock lock];
	LCFrame *f = [[gFrame retain] autorelease];
	[gFrameLock unlock];
	return f;
}

// LCPoll reloads the frame when the file's size or mtime moved. It reports
// whether it did.
static BOOL LCPoll(void) {
	NSString *path = LCPath();
	struct stat st;
	BOOL exists = path && stat(path.fileSystemRepresentation, &st) == 0;
	off_t size = exists ? st.st_size : -1;
	struct timespec mt = exists ? st.st_mtimespec : (struct timespec){0, 0};
	if (gFrame && size == gFileSize && mt.tv_sec == gFileTime.tv_sec && mt.tv_nsec == gFileTime.tv_nsec) {
		return NO;
	}
	gFileSize = size, gFileTime = mt;
	LCFrame *f = [[LCFrame alloc] initWithPath:(exists ? path : nil) generation:++gGeneration];
	[gFrameLock lock];
	LCFrame *old = gFrame;
	gFrame = f;
	[gFrameLock unlock];
	[old release];
	LCLog(@"frame %lu: %@ %zux%zu, %lu QR", (unsigned long)f->generation, f->image ? path : @"(no image)", f->width, f->height,
	      (unsigned long)f->codes.count);
	return YES;
}

#pragma mark - Device

@interface LCFormat : AVCaptureDeviceFormat
@end

@implementation LCFormat
- (AVMediaType)mediaType { return AVMediaTypeVideo; }
- (CMFormatDescriptionRef)formatDescription {
	static CMVideoFormatDescriptionRef desc;
	static dispatch_once_t once;
	dispatch_once(&once, ^{
		CMVideoFormatDescriptionCreate(NULL, kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange, 1920, 1080, NULL, &desc);
	});
	return desc;
}
- (NSArray *)videoSupportedFrameRateRanges { return @[]; }
- (CGFloat)videoMaxZoomFactor { return 16; }
- (float)videoFieldOfView { return 70; }
- (BOOL)isVideoBinned { return NO; }
- (BOOL)isVideoStabilizationModeSupported:(AVCaptureVideoStabilizationMode)mode { return mode == AVCaptureVideoStabilizationModeOff; }
- (CGFloat)videoZoomFactorUpscaleThreshold { return 1; }
- (float)minISO { return 50; }
- (float)maxISO { return 1600; }
- (CMTime)minExposureDuration { return CMTimeMake(1, 1000); }
- (CMTime)maxExposureDuration { return CMTimeMake(1, 30); }
- (NSArray *)supportedMaxPhotoDimensions { return @[]; }
- (NSArray *)secondaryNativeResolutionZoomFactors { return @[]; }
- (BOOL)isHighestPhotoQualitySupported { return NO; }
- (BOOL)isMultiCamSupported { return NO; }
- (NSArray *)supportedColorSpaces { return @[ @(AVCaptureColorSpace_sRGB) ]; }
- (NSString *)description { return @"<loupecam 1920x1080 420v>"; }
@end

@interface LCDevice : AVCaptureDevice {
	CGFloat zoom;
	AVCaptureFocusMode focusMode;
	AVCaptureExposureMode exposureMode;
	AVCaptureWhiteBalanceMode whiteBalanceMode;
	CGPoint focusPoint, exposurePoint;
	AVCaptureAutoFocusRangeRestriction rangeRestriction;
	BOOL lowLight, subjectArea, smoothFocus;
	AVCaptureTorchMode torchMode;
	CMTime minFrame, maxFrame;
}
@end

static LCDevice *gDevice;
static LCFormat *gFormat;

@implementation LCDevice
- (NSString *)uniqueID { return @"dev.bmcreations.loupe.camera.back"; }
- (NSString *)modelID { return @"loupecam"; }
- (NSString *)localizedName { return @"Loupe Camera"; }
- (NSString *)manufacturer { return @"loupe"; }
- (AVCaptureDeviceType)deviceType { return AVCaptureDeviceTypeBuiltInWideAngleCamera; }
- (AVCaptureDevicePosition)position { return AVCaptureDevicePositionBack; }
- (BOOL)hasMediaType:(AVMediaType)t { return [t isEqualToString:AVMediaTypeVideo]; }
- (BOOL)supportsAVCaptureSessionPreset:(AVCaptureSessionPreset)p { return YES; }
- (BOOL)isConnected { return YES; }
- (BOOL)isSuspended { return NO; }
- (BOOL)isInUseByAnotherApplication { return NO; }
- (NSArray *)formats { return @[ gFormat ]; }
- (AVCaptureDeviceFormat *)activeFormat { return gFormat; }
- (void)setActiveFormat:(AVCaptureDeviceFormat *)f {}
- (CMTime)activeVideoMinFrameDuration { return CMTIME_IS_VALID(minFrame) ? minFrame : CMTimeMake(1, 30); }
- (void)setActiveVideoMinFrameDuration:(CMTime)t { minFrame = t; }
- (CMTime)activeVideoMaxFrameDuration { return CMTIME_IS_VALID(maxFrame) ? maxFrame : CMTimeMake(1, 30); }
- (void)setActiveVideoMaxFrameDuration:(CMTime)t { maxFrame = t; }
- (BOOL)lockForConfiguration:(NSError **)error { return YES; }
- (void)unlockForConfiguration {}
- (BOOL)isVirtualDevice { return NO; }
- (NSArray *)constituentDevices { return @[]; }
- (NSArray *)virtualDeviceSwitchOverVideoZoomFactors { return @[]; }
- (CGFloat)videoZoomFactor { return zoom < 1 ? 1 : zoom; }
- (void)setVideoZoomFactor:(CGFloat)z { zoom = z < 1 ? 1 : (z > 16 ? 16 : z); }
- (void)rampToVideoZoomFactor:(CGFloat)z withRate:(float)rate { [self setVideoZoomFactor:z]; }
- (void)cancelVideoZoomRamp {}
- (BOOL)isRampingVideoZoom { return NO; }
- (CGFloat)minAvailableVideoZoomFactor { return 1; }
- (CGFloat)maxAvailableVideoZoomFactor { return 16; }
- (CGFloat)displayVideoZoomFactorMultiplier { return 1; }
- (BOOL)isFocusModeSupported:(AVCaptureFocusMode)m { return YES; }
- (AVCaptureFocusMode)focusMode { return focusMode; }
- (void)setFocusMode:(AVCaptureFocusMode)m { focusMode = m; }
- (BOOL)isFocusPointOfInterestSupported { return YES; }
- (CGPoint)focusPointOfInterest { return focusPoint; }
- (void)setFocusPointOfInterest:(CGPoint)p { focusPoint = p; }
- (BOOL)isAutoFocusRangeRestrictionSupported { return YES; }
- (AVCaptureAutoFocusRangeRestriction)autoFocusRangeRestriction { return rangeRestriction; }
- (void)setAutoFocusRangeRestriction:(AVCaptureAutoFocusRangeRestriction)r { rangeRestriction = r; }
- (BOOL)isSmoothAutoFocusSupported { return YES; }
- (BOOL)isSmoothAutoFocusEnabled { return smoothFocus; }
- (void)setSmoothAutoFocusEnabled:(BOOL)b { smoothFocus = b; }
- (BOOL)isAdjustingFocus { return NO; }
- (float)lensPosition { return 0.5f; }
- (void)setFocusModeLockedWithLensPosition:(float)p completionHandler:(void (^)(CMTime))h {
	if (h) {
		h(CMClockGetTime(CMClockGetHostTimeClock()));
	}
}
- (BOOL)isExposureModeSupported:(AVCaptureExposureMode)m { return YES; }
- (AVCaptureExposureMode)exposureMode { return exposureMode; }
- (void)setExposureMode:(AVCaptureExposureMode)m { exposureMode = m; }
- (BOOL)isExposurePointOfInterestSupported { return YES; }
- (CGPoint)exposurePointOfInterest { return exposurePoint; }
- (void)setExposurePointOfInterest:(CGPoint)p { exposurePoint = p; }
- (BOOL)isAdjustingExposure { return NO; }
- (float)ISO { return 100; }
- (CMTime)exposureDuration { return CMTimeMake(1, 60); }
- (float)exposureTargetBias { return 0; }
- (float)minExposureTargetBias { return -8; }
- (float)maxExposureTargetBias { return 8; }
- (void)setExposureTargetBias:(float)b completionHandler:(void (^)(CMTime))h {
	if (h) {
		h(CMClockGetTime(CMClockGetHostTimeClock()));
	}
}
- (BOOL)isWhiteBalanceModeSupported:(AVCaptureWhiteBalanceMode)m { return YES; }
- (AVCaptureWhiteBalanceMode)whiteBalanceMode { return whiteBalanceMode; }
- (void)setWhiteBalanceMode:(AVCaptureWhiteBalanceMode)m { whiteBalanceMode = m; }
- (BOOL)isAdjustingWhiteBalance { return NO; }
- (BOOL)isLowLightBoostSupported { return NO; }
- (BOOL)isLowLightBoostEnabled { return NO; }
- (BOOL)automaticallyEnablesLowLightBoostWhenAvailable { return lowLight; }
- (void)setAutomaticallyEnablesLowLightBoostWhenAvailable:(BOOL)b { lowLight = b; }
- (BOOL)isSubjectAreaChangeMonitoringEnabled { return subjectArea; }
- (void)setSubjectAreaChangeMonitoringEnabled:(BOOL)b { subjectArea = b; }
- (BOOL)hasTorch { return NO; }
- (BOOL)hasFlash { return NO; }
- (BOOL)isTorchAvailable { return NO; }
- (BOOL)isTorchActive { return NO; }
- (BOOL)isTorchModeSupported:(AVCaptureTorchMode)m { return m == AVCaptureTorchModeOff; }
- (AVCaptureTorchMode)torchMode { return torchMode; }
- (void)setTorchMode:(AVCaptureTorchMode)m { torchMode = m; }
- (BOOL)isFlashAvailable { return NO; }
- (BOOL)isCenterStageActive { return NO; }
- (BOOL)isVideoHDREnabled { return NO; }
- (BOOL)automaticallyAdjustsVideoHDREnabled { return YES; }
- (void)setAutomaticallyAdjustsVideoHDREnabled:(BOOL)b {}
- (BOOL)isGeometricDistortionCorrectionSupported { return NO; }
- (NSString *)description { return @"<loupecam back camera>"; }
- (oneway void)release {}
- (instancetype)retain { return self; }
- (NSUInteger)retainCount { return NSUIntegerMax; }
@end

static BOOL LCWantsDevice(AVMediaType type, AVCaptureDevicePosition pos) {
	return [type isEqualToString:AVMediaTypeVideo] && pos != AVCaptureDevicePositionFront;
}

@implementation AVCaptureDevice (LoupeCam)
+ (AVCaptureDevice *)lc_defaultDeviceWithMediaType:(AVMediaType)t {
	return LCWantsDevice(t, AVCaptureDevicePositionUnspecified) ? gDevice : [self lc_defaultDeviceWithMediaType:t];
}
+ (AVCaptureDevice *)lc_defaultDeviceWithDeviceType:(AVCaptureDeviceType)dt mediaType:(AVMediaType)t position:(AVCaptureDevicePosition)p {
	if (LCWantsDevice(t ?: AVMediaTypeVideo, p) && ![dt isEqualToString:AVCaptureDeviceTypeBuiltInTrueDepthCamera]) {
		return gDevice;
	}
	return [self lc_defaultDeviceWithDeviceType:dt mediaType:t position:p];
}
+ (NSArray *)lc_devicesWithMediaType:(AVMediaType)t {
	NSArray *real = [self lc_devicesWithMediaType:t];
	return [t isEqualToString:AVMediaTypeVideo] ? [@[ gDevice ] arrayByAddingObjectsFromArray:real ?: @[]] : real;
}
+ (NSArray *)lc_devices {
	return [@[ gDevice ] arrayByAddingObjectsFromArray:[self lc_devices] ?: @[]];
}
+ (AVCaptureDevice *)lc_deviceWithUniqueID:(NSString *)uid {
	return [uid isEqualToString:gDevice.uniqueID] ? gDevice : [self lc_deviceWithUniqueID:uid];
}
+ (AVCaptureDevice *)lc_systemPreferredCamera { return gDevice; }
+ (AVAuthorizationStatus)lc_authorizationStatusForMediaType:(AVMediaType)t {
	return [t isEqualToString:AVMediaTypeVideo] ? AVAuthorizationStatusAuthorized : [self lc_authorizationStatusForMediaType:t];
}
+ (void)lc_requestAccessForMediaType:(AVMediaType)t completionHandler:(void (^)(BOOL))handler {
	if (![t isEqualToString:AVMediaTypeVideo]) {
		[self lc_requestAccessForMediaType:t completionHandler:handler];
		return;
	}
	void (^h)(BOOL) = [handler copy];
	dispatch_async(dispatch_get_global_queue(QOS_CLASS_USER_INITIATED, 0), ^{
		h(YES);
		[h release];
	});
}
@end

static const void *kLCDiscoveryWants = &kLCDiscoveryWants;

@implementation AVCaptureDeviceDiscoverySession (LoupeCam)
+ (instancetype)lc_discoverySessionWithDeviceTypes:(NSArray *)types mediaType:(AVMediaType)t position:(AVCaptureDevicePosition)p {
	AVCaptureDeviceDiscoverySession *s = [self lc_discoverySessionWithDeviceTypes:types mediaType:t position:p];
	BOOL wants = LCWantsDevice(t ?: AVMediaTypeVideo, p) && !([types count] == 1 && [types.firstObject isEqualToString:AVCaptureDeviceTypeBuiltInTrueDepthCamera]);
	objc_setAssociatedObject(s, kLCDiscoveryWants, @(wants), OBJC_ASSOCIATION_RETAIN);
	return s;
}
- (NSArray *)lc_devices {
	NSArray *real = [self lc_devices];
	if (![objc_getAssociatedObject(self, kLCDiscoveryWants) boolValue]) {
		return real;
	}
	return [@[ gDevice ] arrayByAddingObjectsFromArray:real ?: @[]];
}
@end

#pragma mark - Input

@interface LCInput : AVCaptureDeviceInput
@end

static LCInput *gInput;

@implementation LCInput
- (AVCaptureDevice *)device { return gDevice; }
- (NSArray *)ports { return @[]; }
- (NSString *)description { return @"<loupecam input>"; }
- (oneway void)release {}
- (instancetype)retain { return self; }
- (NSUInteger)retainCount { return NSUIntegerMax; }
@end

@implementation AVCaptureDeviceInput (LoupeCam)
- (instancetype)lc_initWithDevice:(AVCaptureDevice *)d error:(NSError **)e __attribute__((objc_method_family(init))) {
	if (d == gDevice) {
		[self release];
		return [gInput retain];
	}
	return [self lc_initWithDevice:d error:e];
}
+ (instancetype)lc_deviceInputWithDevice:(AVCaptureDevice *)d error:(NSError **)e {
	return d == gDevice ? gInput : [self lc_deviceInputWithDevice:d error:e];
}
@end

#pragma mark - Connection

@interface LCConnection : AVCaptureConnection {
@public
	AVCaptureOutput *lcOutput; // weak: the output holds the connection
	CGFloat rotation; // clockwise degrees; 0 is the native landscape buffer
	BOOL mirrored, autoMirror, enabled;
}
@end

// LCAngle is a connection's rotation as one of 0, 90, 180 or 270.
static int LCAngle(LCConnection *c) {
	int a = ((int)lround(c ? c->rotation / 90 : 0) % 4 + 4) % 4;
	return a * 90;
}

@implementation LCConnection
- (AVCaptureOutput *)output { return lcOutput; }
- (AVMediaType)mediaType { return AVMediaTypeVideo; }
- (NSArray *)inputPorts { return @[]; }
- (BOOL)isEnabled { return enabled; }
- (void)setEnabled:(BOOL)b { enabled = b; }
- (BOOL)isActive { return YES; }
- (BOOL)isVideoOrientationSupported { return YES; }
- (AVCaptureVideoOrientation)videoOrientation {
	switch (LCAngle(self)) {
	case 90: return AVCaptureVideoOrientationPortrait;
	case 180: return AVCaptureVideoOrientationLandscapeLeft;
	case 270: return AVCaptureVideoOrientationPortraitUpsideDown;
	default: return AVCaptureVideoOrientationLandscapeRight;
	}
}
- (void)setVideoOrientation:(AVCaptureVideoOrientation)o {
	switch (o) {
	case AVCaptureVideoOrientationPortrait: rotation = 90; break;
	case AVCaptureVideoOrientationLandscapeLeft: rotation = 180; break;
	case AVCaptureVideoOrientationPortraitUpsideDown: rotation = 270; break;
	default: rotation = 0; break;
	}
}
- (BOOL)isVideoRotationAngleSupported:(CGFloat)a { return YES; }
- (CGFloat)videoRotationAngle { return rotation; }
- (void)setVideoRotationAngle:(CGFloat)a { rotation = a; }
- (BOOL)isVideoMirroringSupported { return YES; }
- (BOOL)isVideoMirrored { return mirrored; }
- (void)setVideoMirrored:(BOOL)b { mirrored = b; }
- (BOOL)automaticallyAdjustsVideoMirroring { return autoMirror; }
- (void)setAutomaticallyAdjustsVideoMirroring:(BOOL)b { autoMirror = b; }
- (BOOL)isVideoStabilizationSupported { return NO; }
- (AVCaptureVideoStabilizationMode)preferredVideoStabilizationMode { return AVCaptureVideoStabilizationModeOff; }
- (void)setPreferredVideoStabilizationMode:(AVCaptureVideoStabilizationMode)m {}
- (AVCaptureVideoStabilizationMode)activeVideoStabilizationMode { return AVCaptureVideoStabilizationModeOff; }
- (BOOL)isCameraIntrinsicMatrixDeliverySupported { return NO; }
- (CGFloat)videoScaleAndCropFactor { return 1; }
- (CGFloat)videoMaxScaleAndCropFactor { return 1; }
@end

#pragma mark - Outputs

static const void *kLCConnection = &kLCConnection;
static const void *kLCDelegate = &kLCDelegate;
static const void *kLCQueue = &kLCQueue;
static const void *kLCSettings = &kLCSettings;
static const void *kLCTypes = &kLCTypes;
static const void *kLCBusy = &kLCBusy;

// A delegate is held weakly, as AVFoundation holds it.
@interface LCWeak : NSObject {
@public
	id obj;
}
@end
@implementation LCWeak
- (void)dealloc {
	objc_storeWeak(&obj, nil);
	[super dealloc];
}
@end

static void LCSetDelegate(id output, id delegate, dispatch_queue_t queue) {
	LCWeak *w = [LCWeak new];
	objc_storeWeak(&w->obj, delegate);
	objc_setAssociatedObject(output, kLCDelegate, w, OBJC_ASSOCIATION_RETAIN);
	[w release];
	objc_setAssociatedObject(output, kLCQueue, queue, OBJC_ASSOCIATION_RETAIN);
}

static id LCDelegate(id output) {
	LCWeak *w = objc_getAssociatedObject(output, kLCDelegate);
	return w ? objc_loadWeak(&w->obj) : nil; // already autoreleased
}

static LCConnection *LCConnectionFor(AVCaptureOutput *output) {
	LCConnection *c = objc_getAssociatedObject(output, kLCConnection);
	if (!c) {
		c = [LCConnection alloc];
		c->lcOutput = output;
		c->enabled = YES;
		objc_setAssociatedObject(output, kLCConnection, c, OBJC_ASSOCIATION_RETAIN);
		[c release];
	}
	return c;
}

@implementation AVCaptureOutput (LoupeCam)
- (AVCaptureConnection *)lc_connectionWithMediaType:(AVMediaType)t {
	if ([self isKindOfClass:[AVCaptureVideoDataOutput class]] || [self isKindOfClass:[AVCaptureMetadataOutput class]]) {
		return LCConnectionFor(self);
	}
	return [self lc_connectionWithMediaType:t];
}
- (NSArray *)lc_connections {
	if ([self isKindOfClass:[AVCaptureVideoDataOutput class]] || [self isKindOfClass:[AVCaptureMetadataOutput class]]) {
		return @[ LCConnectionFor(self) ];
	}
	return [self lc_connections];
}
@end

@implementation AVCaptureVideoDataOutput (LoupeCam)
- (void)lc_setSampleBufferDelegate:(id)d queue:(dispatch_queue_t)q {
	LCSetDelegate(self, d, q);
	@try {
		[self lc_setSampleBufferDelegate:d queue:q];
	} @catch (NSException *e) {
	}
}
- (id)lc_sampleBufferDelegate { return LCDelegate(self); }
- (dispatch_queue_t)lc_sampleBufferCallbackQueue { return objc_getAssociatedObject(self, kLCQueue); }
- (NSDictionary *)lc_videoSettings {
	NSDictionary *s = objc_getAssociatedObject(self, kLCSettings);
	if (s) {
		return s;
	}
	size_t w, h;
	LCBufferSize(LCAngle(LCConnectionFor(self)), &w, &h);
	return @{
		(id)kCVPixelBufferPixelFormatTypeKey : @(kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange),
		(id)kCVPixelBufferWidthKey : @(w),
		(id)kCVPixelBufferHeightKey : @(h),
	};
}
- (void)lc_setVideoSettings:(NSDictionary *)s {
	objc_setAssociatedObject(self, kLCSettings, s, OBJC_ASSOCIATION_COPY);
}
- (NSArray *)lc_availableVideoPixelFormatTypes {
	return @[
		@(kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange), @(kCVPixelFormatType_420YpCbCr8BiPlanarFullRange),
		@(kCVPixelFormatType_32BGRA)
	];
}
- (NSArray *)lc_availableVideoCVPixelFormatTypes { return [self lc_availableVideoPixelFormatTypes]; }
@end

// LCFormatFor is the pixel format an output asked for, when it is one this
// can make; anything else gets 420v.
static OSType LCFormatFor(AVCaptureVideoDataOutput *o) {
	NSDictionary *s = objc_getAssociatedObject(o, kLCSettings);
	OSType f = [[s objectForKey:(id)kCVPixelBufferPixelFormatTypeKey] unsignedIntValue];
	switch (f) {
	case kCVPixelFormatType_32BGRA:
	case kCVPixelFormatType_420YpCbCr8BiPlanarFullRange:
	case kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange:
		return f;
	default:
		return kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange;
	}
}

@implementation AVCaptureMetadataOutput (LoupeCam)
- (void)lc_setMetadataObjectsDelegate:(id)d queue:(dispatch_queue_t)q {
	LCSetDelegate(self, d, q);
	@try {
		[self lc_setMetadataObjectsDelegate:d queue:q];
	} @catch (NSException *e) {
	}
}
- (id)lc_metadataObjectsDelegate { return LCDelegate(self); }
- (dispatch_queue_t)lc_metadataObjectsCallbackQueue { return objc_getAssociatedObject(self, kLCQueue); }
- (NSArray *)lc_availableMetadataObjectTypes { return @[ AVMetadataObjectTypeQRCode ]; }
- (NSArray *)lc_metadataObjectTypes { return objc_getAssociatedObject(self, kLCTypes) ?: @[]; }
- (void)lc_setMetadataObjectTypes:(NSArray *)types {
	for (id t in types) {
		if (![t isEqual:AVMetadataObjectTypeQRCode]) {
			// What a phone does with a type it cannot offer.
			[NSException raise:NSInvalidArgumentException
			            format:@"*** -[AVCaptureMetadataOutput setMetadataObjectTypes:] Unsupported type found - use -availableMetadataObjectTypes (loupecam offers QR only)"];
		}
	}
	objc_setAssociatedObject(self, kLCTypes, types, OBJC_ASSOCIATION_COPY);
}
@end

#pragma mark - Session

// LCSession is what a session holds instead of the real capture graph.
@interface LCSessionState : NSObject {
@public
	NSMutableArray *inputs;
	NSMutableArray *outputs;
	BOOL running;
}
@end
@implementation LCSessionState
- (instancetype)init {
	if ((self = [super init])) {
		inputs = [NSMutableArray new];
		outputs = [NSMutableArray new];
	}
	return self;
}
- (void)dealloc {
	[inputs release];
	[outputs release];
	[super dealloc];
}
@end

static const void *kLCState = &kLCState;
static NSHashTable *gRunning; // AVCaptureSession, weak; guarded by itself
static NSHashTable *gLayers;  // AVCaptureVideoPreviewLayer, weak; main thread

static LCSessionState *LCState(AVCaptureSession *s) {
	@synchronized(s) {
		LCSessionState *st = objc_getAssociatedObject(s, kLCState);
		if (!st) {
			st = [LCSessionState new];
			objc_setAssociatedObject(s, kLCState, st, OBJC_ASSOCIATION_RETAIN);
			[st release];
		}
		return st;
	}
}

static void LCRefreshLayers(void);

@implementation AVCaptureSession (LoupeCam)
- (BOOL)lc_canAddInput:(AVCaptureInput *)i {
	if (i == gInput) {
		return ![LCState(self)->inputs containsObject:i];
	}
	return [self lc_canAddInput:i];
}
- (void)lc_addInput:(AVCaptureInput *)i {
	if (i != gInput) {
		[self lc_addInput:i];
		return;
	}
	LCSessionState *st = LCState(self);
	@synchronized(self) {
		if (![st->inputs containsObject:i]) {
			[st->inputs addObject:i];
		}
	}
}
- (void)lc_removeInput:(AVCaptureInput *)i {
	if (i != gInput) {
		[self lc_removeInput:i];
		return;
	}
	@synchronized(self) {
		[LCState(self)->inputs removeObject:i];
	}
}
- (NSArray *)lc_inputs {
	@synchronized(self) {
		return [[self lc_inputs] arrayByAddingObjectsFromArray:LCState(self)->inputs];
	}
}
// Outputs never reach the real session: with no real input, the one on a
// simulator refuses a metadata type and never delivers a frame.
- (BOOL)lc_canAddOutput:(AVCaptureOutput *)o {
	@synchronized(self) {
		return ![LCState(self)->outputs containsObject:o];
	}
}
- (void)lc_addOutput:(AVCaptureOutput *)o {
	@synchronized(self) {
		if (![LCState(self)->outputs containsObject:o]) {
			[LCState(self)->outputs addObject:o];
		}
	}
}
- (void)lc_removeOutput:(AVCaptureOutput *)o {
	@synchronized(self) {
		[LCState(self)->outputs removeObject:o];
	}
}
- (NSArray *)lc_outputs {
	@synchronized(self) {
		return [[self lc_outputs] arrayByAddingObjectsFromArray:LCState(self)->outputs];
	}
}
// A session holding the fake input skips the real configuration pass: it
// validates connections it would find on the stand-ins.
static BOOL LCFaked(AVCaptureSession *s) {
	@synchronized(s) {
		return [LCState(s)->inputs count] > 0 || [LCState(s)->outputs count] > 0;
	}
}
- (void)lc_beginConfiguration {
	if (!LCFaked(self)) {
		[self lc_beginConfiguration];
	}
}
- (void)lc_commitConfiguration {
	if (!LCFaked(self)) {
		[self lc_commitConfiguration];
	}
}
- (NSArray *)lc_connections {
	if (!LCFaked(self)) {
		return [self lc_connections];
	}
	NSMutableArray *out = [NSMutableArray array];
	@synchronized(self) {
		for (AVCaptureOutput *o in LCState(self)->outputs) {
			if ([o isKindOfClass:[AVCaptureVideoDataOutput class]] || [o isKindOfClass:[AVCaptureMetadataOutput class]]) {
				[out addObject:LCConnectionFor(o)];
			}
		}
	}
	return out;
}
- (BOOL)lc_isRunning { return LCState(self)->running; }
- (void)lc_startRunning {
	LCSessionState *st = LCState(self);
	if (st->running) {
		return;
	}
	[self willChangeValueForKey:@"running"];
	st->running = YES;
	[self didChangeValueForKey:@"running"];
	@synchronized(gRunning) {
		[gRunning addObject:self];
	}
	[[NSNotificationCenter defaultCenter] postNotificationName:AVCaptureSessionDidStartRunningNotification object:self];
	dispatch_async(dispatch_get_main_queue(), ^{
		LCRefreshLayers();
	});
}
- (void)lc_stopRunning {
	LCSessionState *st = LCState(self);
	if (!st->running) {
		return;
	}
	[self willChangeValueForKey:@"running"];
	st->running = NO;
	[self didChangeValueForKey:@"running"];
	@synchronized(gRunning) {
		[gRunning removeObject:self];
	}
	[[NSNotificationCenter defaultCenter] postNotificationName:AVCaptureSessionDidStopRunningNotification object:self];
	dispatch_async(dispatch_get_main_queue(), ^{
		LCRefreshLayers();
	});
}
@end

#pragma mark - Preview

static const void *kLCLayer = &kLCLayer;

static NSString *LCGravity(AVLayerVideoGravity g) {
	if ([g isEqualToString:AVLayerVideoGravityResizeAspect]) {
		return kCAGravityResizeAspect;
	}
	if ([g isEqualToString:AVLayerVideoGravityResize]) {
		return kCAGravityResize;
	}
	return kCAGravityResizeAspectFill;
}

// LCRefreshLayer puts the frame in a sublayer of the preview layer, on top of
// whatever AVFoundation draws there, for as long as its session runs.
static void LCRefreshLayer(AVCaptureVideoPreviewLayer *layer) {
	CALayer *img = objc_getAssociatedObject(layer, kLCLayer);
	if (!img) {
		img = [CALayer layer];
		img.masksToBounds = YES;
		objc_setAssociatedObject(layer, kLCLayer, img, OBJC_ASSOCIATION_RETAIN);
	}
	[CATransaction begin];
	[CATransaction setDisableActions:YES];
	if (img.superlayer != layer || layer.sublayers.lastObject != img) {
		[img removeFromSuperlayer];
		[layer addSublayer:img];
	}
	img.frame = layer.bounds;
	img.contentsGravity = LCGravity(layer.videoGravity);
	AVCaptureSession *s = layer.session;
	BOOL live = s && LCState(s)->running;
	LCFrame *f = LCCurrentFrame();
	img.contents = live && f ? (id)f->image : nil;
	static CGColorRef black;
	if (!black) {
		black = CGColorCreateSRGB(0, 0, 0, 1);
	}
	img.backgroundColor = live ? black : NULL;
	img.hidden = !live;
	[CATransaction commit];
}

static void LCRefreshLayers(void) {
	for (AVCaptureVideoPreviewLayer *l in [gLayers allObjects]) {
		LCRefreshLayer(l);
	}
}

@implementation AVCaptureVideoPreviewLayer (LoupeCam)
- (void)lc_track {
	if ([NSThread isMainThread]) {
		[gLayers addObject:self];
		LCRefreshLayer(self);
	} else {
		[self retain];
		dispatch_async(dispatch_get_main_queue(), ^{
			[gLayers addObject:self];
			LCRefreshLayer(self);
			[self release];
		});
	}
}
- (void)lc_setSession:(AVCaptureSession *)s {
	@try {
		[self lc_setSession:s];
	} @catch (NSException *e) {
	}
	[self lc_track];
}
- (void)lc_setSessionWithNoConnection:(AVCaptureSession *)s {
	@try {
		[self lc_setSessionWithNoConnection:s];
	} @catch (NSException *e) {
	}
	[self lc_track];
}
- (instancetype)lc_initWithSession:(AVCaptureSession *)s __attribute__((objc_method_family(init))) {
	self = [self lc_initWithSession:s];
	[self lc_track];
	return self;
}
- (void)lc_layoutSublayers {
	[self lc_layoutSublayers];
	CALayer *img = objc_getAssociatedObject(self, kLCLayer);
	if (img) {
		[CATransaction begin];
		[CATransaction setDisableActions:YES];
		img.frame = self.bounds;
		[CATransaction commit];
	}
}
- (void)lc_setVideoGravity:(AVLayerVideoGravity)g {
	[self lc_setVideoGravity:g];
	CALayer *img = objc_getAssociatedObject(self, kLCLayer);
	img.contentsGravity = LCGravity(g);
}
@end

#pragma mark - Pump

// LCDeliver hands one frame to an output's delegate on its queue. Like a
// phone with alwaysDiscardsLateVideoFrames, it drops a frame while the
// delegate is still busy with the last one.
static void LCDeliver(AVCaptureVideoDataOutput *o, LCFrame *f, CMTime pts) {
	id d = LCDelegate(o);
	dispatch_queue_t q = objc_getAssociatedObject(o, kLCQueue);
	if (!d || !q || ![d respondsToSelector:@selector(captureOutput:didOutputSampleBuffer:fromConnection:)]) {
		return;
	}
	if ([objc_getAssociatedObject(o, kLCBusy) boolValue]) {
		return;
	}
	LCConnection *c = LCConnectionFor(o);
	CVPixelBufferRef pb = [f bufferForFormat:LCFormatFor(o) angle:LCAngle(c)];
	if (!pb) {
		return;
	}
	CMVideoFormatDescriptionRef desc = NULL;
	if (CMVideoFormatDescriptionCreateForImageBuffer(NULL, pb, &desc) != noErr) {
		return;
	}
	CMSampleTimingInfo timing = {.duration = CMTimeMake(1, 30), .presentationTimeStamp = pts, .decodeTimeStamp = kCMTimeInvalid};
	CMSampleBufferRef sb = NULL;
	OSStatus err = CMSampleBufferCreateReadyWithImageBuffer(NULL, pb, desc, &timing, &sb);
	CFRelease(desc);
	if (err != noErr) {
		return;
	}
	objc_setAssociatedObject(o, kLCBusy, @YES, OBJC_ASSOCIATION_RETAIN);
	[d retain];
	[o retain];
	dispatch_async(q, ^{
		@autoreleasepool {
			[(id<AVCaptureVideoDataOutputSampleBufferDelegate>)d captureOutput:o didOutputSampleBuffer:sb fromConnection:c];
		}
		CFRelease(sb);
		objc_setAssociatedObject(o, kLCBusy, @NO, OBJC_ASSOCIATION_RETAIN);
		[d release];
		[o release];
	});
}

static void LCDeliverCodes(AVCaptureMetadataOutput *o, LCFrame *f) {
	NSArray *types = objc_getAssociatedObject(o, kLCTypes);
	if (![types containsObject:AVMetadataObjectTypeQRCode] || f->codes.count == 0) {
		return;
	}
	id d = LCDelegate(o);
	dispatch_queue_t q = objc_getAssociatedObject(o, kLCQueue);
	if (!d || !q || ![d respondsToSelector:@selector(captureOutput:didOutputMetadataObjects:fromConnection:)]) {
		return;
	}
	NSArray *codes = [f->codes retain];
	LCConnection *c = LCConnectionFor(o);
	[d retain];
	[o retain];
	dispatch_async(q, ^{
		@autoreleasepool {
			[(id<AVCaptureMetadataOutputObjectsDelegate>)d captureOutput:o didOutputMetadataObjects:codes fromConnection:c];
		}
		[codes release];
		[d release];
		[o release];
	});
}

static void LCTick(uint64_t n) {
	@autoreleasepool {
		if (n % 8 == 0 && LCPoll()) {
			dispatch_async(dispatch_get_main_queue(), ^{
				LCRefreshLayers();
			});
		}
		NSArray *sessions;
		@synchronized(gRunning) {
			sessions = [gRunning allObjects];
		}
		if (sessions.count == 0) {
			return;
		}
		LCFrame *f = LCCurrentFrame();
		CMTime pts = CMClockGetTime(CMClockGetHostTimeClock());
		for (AVCaptureSession *s in sessions) {
			NSArray *outputs;
			@synchronized(s) {
				outputs = [[LCState(s)->outputs copy] autorelease];
			}
			for (AVCaptureOutput *o in outputs) {
				if ([o isKindOfClass:[AVCaptureVideoDataOutput class]]) {
					LCDeliver((AVCaptureVideoDataOutput *)o, f, pts);
				} else if ([o isKindOfClass:[AVCaptureMetadataOutput class]] && n % 3 == 0) {
					LCDeliverCodes((AVCaptureMetadataOutput *)o, f);
				}
			}
		}
	}
}

#pragma mark - Install

__attribute__((constructor)) static void LCInstall(void) {
	@autoreleasepool {
		gFrameLock = [NSLock new];
		gRunning = [[NSHashTable weakObjectsHashTable] retain];
		gLayers = [[NSHashTable weakObjectsHashTable] retain];
		gFormat = [LCFormat alloc];
		gDevice = [LCDevice alloc];
		gInput = [LCInput alloc];
		LCPoll();

		Class dev = [AVCaptureDevice class];
		LCSwapClass(dev, @selector(defaultDeviceWithMediaType:), @selector(lc_defaultDeviceWithMediaType:));
		LCSwapClass(dev, @selector(defaultDeviceWithDeviceType:mediaType:position:), @selector(lc_defaultDeviceWithDeviceType:mediaType:position:));
		LCSwapClass(dev, @selector(devicesWithMediaType:), @selector(lc_devicesWithMediaType:));
		LCSwapClass(dev, @selector(devices), @selector(lc_devices));
		LCSwapClass(dev, @selector(deviceWithUniqueID:), @selector(lc_deviceWithUniqueID:));
		if ([dev respondsToSelector:@selector(systemPreferredCamera)]) {
			LCSwapClass(dev, @selector(systemPreferredCamera), @selector(lc_systemPreferredCamera));
		}
		LCSwapClass(dev, @selector(authorizationStatusForMediaType:), @selector(lc_authorizationStatusForMediaType:));
		LCSwapClass(dev, @selector(requestAccessForMediaType:completionHandler:), @selector(lc_requestAccessForMediaType:completionHandler:));

		Class disc = [AVCaptureDeviceDiscoverySession class];
		LCSwapClass(disc, @selector(discoverySessionWithDeviceTypes:mediaType:position:), @selector(lc_discoverySessionWithDeviceTypes:mediaType:position:));
		LCSwap(disc, @selector(devices), @selector(lc_devices));

		Class in = [AVCaptureDeviceInput class];
		LCSwap(in, @selector(initWithDevice:error:), @selector(lc_initWithDevice:error:));
		LCSwapClass(in, @selector(deviceInputWithDevice:error:), @selector(lc_deviceInputWithDevice:error:));

		Class out = [AVCaptureOutput class];
		LCSwap(out, @selector(connectionWithMediaType:), @selector(lc_connectionWithMediaType:));
		LCSwap(out, @selector(connections), @selector(lc_connections));

		Class vdo = [AVCaptureVideoDataOutput class];
		LCSwap(vdo, @selector(setSampleBufferDelegate:queue:), @selector(lc_setSampleBufferDelegate:queue:));
		LCSwap(vdo, @selector(sampleBufferDelegate), @selector(lc_sampleBufferDelegate));
		LCSwap(vdo, @selector(sampleBufferCallbackQueue), @selector(lc_sampleBufferCallbackQueue));
		LCSwap(vdo, @selector(videoSettings), @selector(lc_videoSettings));
		LCSwap(vdo, @selector(setVideoSettings:), @selector(lc_setVideoSettings:));
		LCSwap(vdo, @selector(availableVideoPixelFormatTypes), @selector(lc_availableVideoPixelFormatTypes));
		LCSwap(vdo, @selector(availableVideoCVPixelFormatTypes), @selector(lc_availableVideoCVPixelFormatTypes));

		Class mdo = [AVCaptureMetadataOutput class];
		LCSwap(mdo, @selector(setMetadataObjectsDelegate:queue:), @selector(lc_setMetadataObjectsDelegate:queue:));
		LCSwap(mdo, @selector(metadataObjectsDelegate), @selector(lc_metadataObjectsDelegate));
		LCSwap(mdo, @selector(metadataObjectsCallbackQueue), @selector(lc_metadataObjectsCallbackQueue));
		LCSwap(mdo, @selector(availableMetadataObjectTypes), @selector(lc_availableMetadataObjectTypes));
		LCSwap(mdo, @selector(metadataObjectTypes), @selector(lc_metadataObjectTypes));
		LCSwap(mdo, @selector(setMetadataObjectTypes:), @selector(lc_setMetadataObjectTypes:));

		Class ses = [AVCaptureSession class];
		LCSwap(ses, @selector(canAddInput:), @selector(lc_canAddInput:));
		LCSwap(ses, @selector(addInput:), @selector(lc_addInput:));
		LCSwap(ses, @selector(removeInput:), @selector(lc_removeInput:));
		LCSwap(ses, @selector(inputs), @selector(lc_inputs));
		LCSwap(ses, @selector(canAddOutput:), @selector(lc_canAddOutput:));
		LCSwap(ses, @selector(addOutput:), @selector(lc_addOutput:));
		LCSwap(ses, @selector(removeOutput:), @selector(lc_removeOutput:));
		LCSwap(ses, @selector(outputs), @selector(lc_outputs));
		LCSwap(ses, @selector(beginConfiguration), @selector(lc_beginConfiguration));
		LCSwap(ses, @selector(commitConfiguration), @selector(lc_commitConfiguration));
		LCSwap(ses, @selector(connections), @selector(lc_connections));
		LCSwap(ses, @selector(isRunning), @selector(lc_isRunning));
		LCSwap(ses, @selector(startRunning), @selector(lc_startRunning));
		LCSwap(ses, @selector(stopRunning), @selector(lc_stopRunning));

		Class pl = [AVCaptureVideoPreviewLayer class];
		LCSwap(pl, @selector(setSession:), @selector(lc_setSession:));
		LCSwap(pl, @selector(setSessionWithNoConnection:), @selector(lc_setSessionWithNoConnection:));
		LCSwap(pl, @selector(initWithSession:), @selector(lc_initWithSession:));
		LCSwap(pl, @selector(layoutSublayers), @selector(lc_layoutSublayers));
		LCSwap(pl, @selector(setVideoGravity:), @selector(lc_setVideoGravity:));

		dispatch_queue_t q = dispatch_queue_create("dev.bmcreations.loupe.camera", DISPATCH_QUEUE_SERIAL);
		dispatch_source_t timer = dispatch_source_create(DISPATCH_SOURCE_TYPE_TIMER, 0, 0, q);
		dispatch_source_set_timer(timer, dispatch_time(DISPATCH_TIME_NOW, 0), NSEC_PER_SEC / 30, NSEC_PER_MSEC);
		__block uint64_t n = 0;
		dispatch_source_set_event_handler(timer, ^{
			LCTick(n++);
		});
		dispatch_resume(timer);
		LCLog(@"installed, file %@", LCPath());
		// Loupe reads this back to tell that the running app loaded the shim:
		// dyld drops an insert it cannot load without a word.
		NSString *marker = [LCPath() stringByAppendingString:@".loaded"];
		[[NSString stringWithFormat:@"%d\n", getpid()] writeToFile:marker atomically:YES encoding:NSUTF8StringEncoding error:NULL];
	}
}
