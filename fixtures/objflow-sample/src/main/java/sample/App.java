package sample;

public class App {
    public Receipt build(String coupon, int amount) {
        Receipt r = new Receipt();
        new CodeStage().apply(r, coupon);
        if (amount >= 100) {
            new GoldLabelStage().apply(r, amount);
        } else {
            new LabelStage().apply(r, amount);
        }
        new TierStage().apply(r, amount);
        return r;
    }

    public String summary(String coupon, int amount) {
        Receipt r = build(coupon, amount);
        return r.getCode() + "/" + r.getLabel() + "/" + r.getTier();
    }
}
