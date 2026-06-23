package sample;

public class PreviewService {
    public String preview(String coupon) {
        Receipt demo = new Receipt("DEMO");
        demo.setLabel("PREVIEW");
        demo.setTier("NONE");
        return demo.getCode() + "/" + demo.getLabel() + "/" + demo.getTier();
    }
}
