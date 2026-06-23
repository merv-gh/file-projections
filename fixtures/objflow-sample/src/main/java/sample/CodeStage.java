package sample;

public class CodeStage {
    public void apply(Receipt r, String coupon) {
        r.setCode(coupon.toUpperCase());
    }
}
